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
		return fmt.Errorf("EncryptFile: at least one recipient is required")
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening %q: %w", srcPath, err)
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("creating %q: %w", dstPath, err)
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
		return fmt.Errorf("DecryptFile: at least one identity is required")
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

	dst, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("creating %q: %w", dstPath, err)
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
	if err = dst.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", dstPath, err)
	}
	return nil
}
