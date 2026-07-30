package archive

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"filippo.io/age"
)

// ageIntro is the fixed first line of every age-encrypted file, per the
// age-encryption.org/v1 spec (a public wire format, not a library internal).
// Sniffing it lets Open distinguish an encrypted .kshrk archive from a plain
// ZIP one before any key material is available.
const ageIntro = "age-encryption.org/v1\n"

// isAgeEncrypted reports whether f's contents start with the age intro line.
// It uses ReadAt so it does not disturb any other reader of f.
func isAgeEncrypted(f *os.File) (bool, error) {
	buf := make([]byte, len(ageIntro))
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return n == len(buf) && string(buf) == ageIntro, nil
}

// IsEncrypted reports whether the archive at path is an age-encrypted k8shark
// archive (as written by NewEncryptedStreamWriter), reading only the file
// header. A plaintext ZIP archive returns false. Callers use it to decide
// whether a decryption key is needed before opening.
func IsEncrypted(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	return isAgeEncrypted(f)
}

// RecipientsFromPassphrase returns a recipient set for passphrase-based
// archive encryption. Per the age spec a ScryptRecipient must be the only
// recipient for a file, so this is always a single-element slice.
func RecipientsFromPassphrase(passphrase string) ([]age.Recipient, error) {
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, fmt.Errorf("building passphrase recipient: %w", err)
	}
	return []age.Recipient{r}, nil
}

// IdentitiesFromPassphrase returns an identity set for decrypting an archive
// that was encrypted with RecipientsFromPassphrase.
func IdentitiesFromPassphrase(passphrase string) ([]age.Identity, error) {
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, fmt.Errorf("building passphrase identity: %w", err)
	}
	return []age.Identity{id}, nil
}

// isNoIdentityMatch reports whether err is (or wraps) age's
// NoIdentityMatchError, i.e. the supplied key material didn't decrypt the
// archive.
func isNoIdentityMatch(err error) bool {
	var noMatch *age.NoIdentityMatchError
	return errors.As(err, &noMatch)
}

// classifyDecryptCopyError turns an io.Copy failure encountered while
// decrypting into a clear error, distinguishing a genuine OS-level I/O
// problem (disk full or a permission problem writing the temp file, a read
// error on the source) from age's per-chunk STREAM authentication rejecting
// tampered or corrupt ciphertext — conflating the two would make an ordinary
// operational failure look like a security incident. A wrong
// identity/passphrase is always caught earlier, synchronously in
// age.Decrypt's header parsing, so anything reaching here that isn't an I/O
// error is a tamper/corruption signal.
func classifyDecryptCopyError(srcPath string, err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("decrypting \"%s\": %w", srcPath, err)
	}
	return fmt.Errorf("decrypting \"%s\": tampered or corrupt archive: %w", srcPath, err)
}

// createLike creates a fresh temporary file in dstPath's directory — never
// dstPath itself — and returns it along with src's permission bits and a
// commit function. The caller writes into the returned file, then, once
// finished, calls commit to fix its permissions to match src exactly and
// atomically rename it into place as dstPath; on error, the caller instead
// closes the returned file and os.Remove's its Name() to discard it (dstPath
// itself is never touched either way). EncryptFile/DecryptFile use this so a
// source archive stored with restrictive permissions (e.g. 0600) can't
// silently turn into a more permissive encrypted or plaintext copy — this
// matters most for DecryptFile, whose output is plaintext.
//
// Writing to a same-directory temp file and renaming into place, rather than
// opening dstPath directly, closes two related gaps a direct-open approach
// (an earlier version of this function used one) can't avoid:
//   - Partially-written output is never observable at dstPath: on any
//     failure the temp file is simply discarded, never having touched
//     whatever (if anything) previously existed at dstPath.
//   - Symlink-following, safely, even under a TOCTOU race: an Lstat-then-Open
//     check on dstPath (what an earlier version of this function did) can
//     still be raced — dstPath can be swapped for a symlink after the check
//     and before the open, causing a direct write to follow the link and
//     corrupt an unintended target. os.Rename doesn't have this problem:
//     POSIX rename(2) atomically replaces whatever is *named* dstPath — if
//     that's a symlink, the symlink itself is replaced, never its target —
//     no matter when dstPath started (or stopped) being a symlink relative
//     to this function's checks.
//
// An existing symlink or other non-regular file at dstPath is still rejected
// up front with a clear error, rather than silently replacing it.
func createLike(dstPath string, src *os.File) (dst *os.File, mode os.FileMode, commit func() error, err error) {
	srcInfo, err := src.Stat()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("stat \"%s\": %w", src.Name(), err)
	}
	mode = srcInfo.Mode().Perm()

	switch existing, statErr := os.Lstat(dstPath); {
	case statErr == nil:
		if !existing.Mode().IsRegular() {
			return nil, 0, nil, fmt.Errorf("output \"%s\" exists and is not a regular file (mode %s)", dstPath, existing.Mode())
		}
		if os.SameFile(srcInfo, existing) {
			return nil, 0, nil, fmt.Errorf("output \"%s\" must not be the same file as the input", dstPath)
		}
	case errors.Is(statErr, fs.ErrNotExist):
		// The common case: no output exists yet, nothing to check.
	default:
		// Anything else (permission denied traversing a parent directory, an
		// I/O error, ...) is a real failure — don't silently treat it the
		// same as "doesn't exist" and proceed without having enforced the
		// non-regular/same-file checks above.
		return nil, 0, nil, fmt.Errorf("checking existing output \"%s\": %w", dstPath, statErr)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dstPath), "."+filepath.Base(dstPath)+".tmp-*")
	if err != nil {
		return nil, 0, nil, fmt.Errorf("creating temporary file for \"%s\": %w", dstPath, err)
	}
	tmpPath := tmp.Name()
	// Deliberately left at os.CreateTemp's default 0600 (not widened to
	// src's mode) for the entire time the file is being populated: 0600
	// already grants the owner-write this function needs regardless of what
	// src's mode is, and NOT touching it here means a permissive source mode
	// (e.g. 0644) can never cause partially-written content — plaintext, for
	// DecryptFile — to sit exposed at that wider mode while being written or
	// if cleanup runs after a failure. commit is the only place the file's
	// mode changes, narrowing (or widening) it to the exact source mode in
	// the same moment it's renamed into place.

	commit = func() error {
		if err := tmp.Chmod(mode); err != nil {
			return fmt.Errorf("setting permissions on \"%s\": %w", tmpPath, err)
		}
		if err := tmp.Close(); err != nil {
			return fmt.Errorf("closing \"%s\": %w", tmpPath, err)
		}
		if err := os.Rename(tmpPath, dstPath); err != nil {
			return fmt.Errorf("renaming temporary file into place as \"%s\": %w", dstPath, err)
		}
		return nil
	}
	return tmp, mode, commit, nil
}

// EncryptFile writes an age-encrypted copy of the plaintext file at srcPath to
// dstPath, encrypting to recipients. Unlike NewEncryptedStreamWriter (which
// encrypts while writing a brand-new archive), this is a whole-file transform
// for encrypting an archive that already exists on disk — used by
// `kshrk encrypt`. It refuses to run if srcPath is already an age-encrypted
// file. Writing happens via createLike's temp-file-then-rename, so dstPath is
// never partially written: on any failure the discarded temp file is removed
// and dstPath (whether or not it already existed) is left untouched.
func EncryptFile(srcPath, dstPath string, recipients []age.Recipient) (err error) {
	encrypted, err := IsEncrypted(srcPath)
	if err != nil {
		return fmt.Errorf("reading \"%s\": %w", srcPath, err)
	}
	if encrypted {
		return fmt.Errorf("\"%s\" is already encrypted", srcPath)
	}
	if len(recipients) == 0 {
		return fmt.Errorf("archive encryption requires at least one recipient")
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening \"%s\": %w", srcPath, err)
	}
	defer src.Close()

	dst, _, commit, err := createLike(dstPath, src)
	if err != nil {
		return err
	}
	tmpPath := dst.Name()
	// Every failure from here on discards the temporary file, which never
	// touched dstPath — so each branch below only needs to set err and
	// return. Once commit succeeds this is a no-op (err is nil).
	defer func() {
		if err != nil {
			dst.Close()
			os.Remove(tmpPath)
		}
	}()

	w, err := age.Encrypt(dst, recipients...)
	if err != nil {
		return fmt.Errorf("setting up encryption: %w", err)
	}
	if _, err = io.Copy(w, src); err != nil {
		_ = w.Close() // best-effort: attempt to finalize before the deferred cleanup discards the temp file
		return fmt.Errorf("encrypting \"%s\": %w", srcPath, err)
	}
	if err = w.Close(); err != nil {
		return fmt.Errorf("finishing encryption of \"%s\": %w", srcPath, err)
	}
	if err = commit(); err != nil {
		return err
	}
	return nil
}

// DecryptFile writes a plaintext copy of the age-encrypted file at srcPath to
// dstPath, decrypting with identities. It is the inverse whole-file transform
// of EncryptFile, used by `kshrk decrypt`. It refuses to run if srcPath is not
// an age-encrypted file. Writing happens via createLike's
// temp-file-then-rename, so dstPath is never partially written: on any
// failure the discarded temp file is removed and dstPath (whether or not it
// already existed) is left untouched.
func DecryptFile(srcPath, dstPath string, identities []age.Identity) (err error) {
	encrypted, err := IsEncrypted(srcPath)
	if err != nil {
		return fmt.Errorf("reading \"%s\": %w", srcPath, err)
	}
	if !encrypted {
		return fmt.Errorf("\"%s\" is not encrypted", srcPath)
	}
	if len(identities) == 0 {
		return fmt.Errorf("archive decryption requires at least one identity")
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening \"%s\": %w", srcPath, err)
	}
	defer src.Close()

	r, err := age.Decrypt(src, identities...)
	if err != nil {
		if isNoIdentityMatch(err) {
			return fmt.Errorf("failed to decrypt \"%s\": incorrect passphrase or key", srcPath)
		}
		return fmt.Errorf("decrypting \"%s\": %w", srcPath, err)
	}

	dst, _, commit, err := createLike(dstPath, src)
	if err != nil {
		return err
	}
	tmpPath := dst.Name()
	// Every failure from here on discards the temporary file, which never
	// touched dstPath — so each branch below only needs to set err and
	// return. Once commit succeeds this is a no-op (err is nil).
	defer func() {
		if err != nil {
			dst.Close()
			os.Remove(tmpPath)
		}
	}()

	if _, err = io.Copy(dst, r); err != nil {
		return classifyDecryptCopyError(srcPath, err)
	}
	if err = commit(); err != nil {
		return err
	}
	return nil
}
