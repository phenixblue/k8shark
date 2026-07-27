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
