package archive

import (
	"errors"
	"fmt"
	"io"
	"os"

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

// createLike creates dstPath and returns it along with src's permission bits,
// instead of os.Create's fixed 0666&umask. EncryptFile/DecryptFile can turn a
// source archive stored with restrictive permissions (e.g. 0600) into an
// encrypted or plaintext copy, so the copy shouldn't silently end up more
// permissive than the original — this matters most for DecryptFile, whose
// output is plaintext.
//
// dstPath is locked to mode|0o200 (the source's permission bits, plus the
// owner-write needed to populate it) before this function returns — never to
// whatever an existing dstPath's own mode happened to be. That matters when
// dstPath already exists more permissively than the source (e.g. a stale
// 0644 file where the source is 0600): narrowing it only via a final Chmod
// after writing (as an earlier version of this function did) would leave
// plaintext sitting under the loose permissions for the entire duration of
// the write. Permission failures here are fatal, not best-effort — if the
// intended mode can't be set, no content is written under the wrong
// permissions in the first place. Callers still restore the exact source
// mode (dropping the extra owner-write bit, via (*os.File).Chmod using the
// returned FileMode) once they're done writing, before the final Close.
func createLike(dstPath string, src *os.File) (*os.File, os.FileMode, error) {
	srcInfo, err := src.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("stat %q: %w", src.Name(), err)
	}
	mode := srcInfo.Mode().Perm()

	if existing, statErr := os.Stat(dstPath); statErr == nil {
		if os.SameFile(srcInfo, existing) {
			return nil, 0, fmt.Errorf("output %q must not be the same file as the input", dstPath)
		}
		// Only touch the mode of a regular file — dstPath could be a
		// directory or other special file (the caller passed a bad --output),
		// and chmod'ing that isn't this function's business; let the
		// OpenFile below report a clear error for it instead. This also
		// narrows an existing, more-permissive dstPath down to the intended
		// mode *before* opening it, so there is no window where it sits at
		// looser-than-intended permissions while content is written.
		if existing.Mode().IsRegular() {
			if err := os.Chmod(dstPath, mode|0o200); err != nil {
				return nil, 0, fmt.Errorf("setting permissions on %q: %w", dstPath, err)
			}
		}
	}
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode|0o200)
	if err != nil {
		return nil, 0, fmt.Errorf("creating %q: %w", dstPath, err)
	}
	// This is the call that actually closes the loose-permission window: an
	// existing dstPath that was open()-able because it already had the
	// owner-write bit (e.g. a stale, more-permissive 0644 where the source is
	// 0600) is narrowed here, before any content is written — not left at its
	// old, looser mode until the caller's chmod after writing finishes. It
	// also covers OpenFile's create mode being subject to umask, for a
	// freshly-created dstPath.
	if err := dst.Chmod(mode | 0o200); err != nil {
		dst.Close()
		os.Remove(dstPath)
		return nil, 0, fmt.Errorf("setting permissions on %q: %w", dstPath, err)
	}
	return dst, mode, nil
}

// EncryptFile writes an age-encrypted copy of the plaintext file at srcPath to
// dstPath, encrypting to recipients. Unlike NewEncryptedStreamWriter (which
// encrypts while writing a brand-new archive), this is a whole-file transform
// for encrypting an archive that already exists on disk — used by
// `kshrk encrypt`. It refuses to run if srcPath is already an age-encrypted
// file, and removes a partially-written dstPath on any failure.
func EncryptFile(srcPath, dstPath string, recipients []age.Recipient) (err error) {
	encrypted, err := IsEncrypted(srcPath)
	if err != nil {
		return fmt.Errorf("reading %q: %w", srcPath, err)
	}
	if encrypted {
		return fmt.Errorf("%q is already encrypted", srcPath)
	}
	if len(recipients) == 0 {
		return fmt.Errorf("archive encryption requires at least one recipient")
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening %q: %w", srcPath, err)
	}
	defer src.Close()

	dst, mode, err := createLike(dstPath, src)
	if err != nil {
		return err
	}
	// Every failure from here on removes the partially-written output, so
	// each branch below only needs to set err and return — no repeated
	// close/remove boilerplate to keep in sync.
	defer func() {
		if err != nil {
			dst.Close()
			os.Remove(dstPath)
		}
	}()

	w, err := age.Encrypt(dst, recipients...)
	if err != nil {
		return fmt.Errorf("setting up encryption: %w", err)
	}
	if _, err = io.Copy(w, src); err != nil {
		_ = w.Close() // best-effort: attempt to finalize before the deferred cleanup removes dstPath
		return fmt.Errorf("encrypting %q: %w", srcPath, err)
	}
	if err = w.Close(); err != nil {
		return fmt.Errorf("finishing encryption of %q: %w", srcPath, err)
	}
	// Restore the source's exact mode now that writing is done — createLike
	// forced the owner-write bit on so the writes above could happen at all.
	if err = dst.Chmod(mode); err != nil {
		return fmt.Errorf("setting permissions on %q: %w", dstPath, err)
	}
	if err = dst.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", dstPath, err)
	}
	return nil
}

// DecryptFile writes a plaintext copy of the age-encrypted file at srcPath to
// dstPath, decrypting with identities. It is the inverse whole-file transform
// of EncryptFile, used by `kshrk decrypt`. It refuses to run if srcPath is not
// an age-encrypted file, and removes a partially-written dstPath on any
// failure.
func DecryptFile(srcPath, dstPath string, identities []age.Identity) (err error) {
	encrypted, err := IsEncrypted(srcPath)
	if err != nil {
		return fmt.Errorf("reading %q: %w", srcPath, err)
	}
	if !encrypted {
		return fmt.Errorf("%q is not encrypted", srcPath)
	}
	if len(identities) == 0 {
		return fmt.Errorf("archive decryption requires at least one identity")
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening %q: %w", srcPath, err)
	}
	defer src.Close()

	r, err := age.Decrypt(src, identities...)
	if err != nil {
		if isNoIdentityMatch(err) {
			return fmt.Errorf("failed to decrypt %q: incorrect passphrase or key", srcPath)
		}
		return fmt.Errorf("decrypting %q: %w", srcPath, err)
	}

	dst, mode, err := createLike(dstPath, src)
	if err != nil {
		return err
	}
	// Every failure from here on removes the partially-written output, so
	// each branch below only needs to set err and return — no repeated
	// close/remove boilerplate to keep in sync.
	defer func() {
		if err != nil {
			dst.Close()
			os.Remove(dstPath)
		}
	}()

	if _, err = io.Copy(dst, r); err != nil {
		// A wrong identity/passphrase is always caught above, synchronously in
		// age.Decrypt's header parsing. A failure here means age's per-chunk
		// STREAM authentication rejected tampered or corrupt ciphertext.
		return fmt.Errorf("decrypting %q: tampered or corrupt archive: %w", srcPath, err)
	}
	// Restore the source's exact mode now that writing is done — createLike
	// forced the owner-write bit on so the write above could happen at all.
	if err = dst.Chmod(mode); err != nil {
		return fmt.Errorf("setting permissions on %q: %w", dstPath, err)
	}
	if err = dst.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", dstPath, err)
	}
	return nil
}
