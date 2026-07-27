# Encryption threat model

`.kshrk` captures can contain Kubernetes Secret values, tokens, and other
cluster PII. `kshrk capture`'s encryption flags (`--encrypt`,
`--encrypt-passphrase-file`, `--encrypt-recipient`,
`--encrypt-recipients-file` — and the equivalent `kshrk redact` flags) let you
write an archive as a single [age](https://age-encryption.org/v1) envelope.
This page states plainly what that protects, what it doesn't, and the
key-handling rules the CLI enforces.
See [archive-format.md](archive-format.md#encryption) for how the envelope is
built; see [usage.md](usage.md#encryption) for command examples.

## What is protected

- **Everything inside the archive at rest**: record bodies (including Secret
  data), the index, the watch index, and `metadata.json` are all inside a
  single age ciphertext. There is no partial-plaintext archive — either you
  have a working key or you have opaque bytes.
- **Archive structure**, not just content: because encryption wraps the whole
  ZIP container rather than individual entries, an attacker who obtains the
  file cannot see entry names (which would otherwise be SHA-256-derived
  directory names), per-record sizes, or the record count from the ZIP
  central directory. This is deliberately stronger than a per-entry encryption
  design would have been, at the cost described below.
- **Tamper detection**: age's AEAD construction (ChaCha20-Poly1305 under the
  STREAM chunking) authenticates every chunk. Any bit flipped in the
  ciphertext — accidental corruption or deliberate tampering — causes
  decryption to fail outright rather than silently returning corrupted data.
- **In-transit copies**: since the whole file is one opaque envelope, copying
  it over email, object storage, a support ticket attachment, etc. carries no
  additional exposure beyond having the ciphertext bytes.

## What is NOT protected (explicitly out of scope)

- **File existence and approximate size.** Anyone with filesystem or network
  access to the `.kshrk` file can see that it exists, its approximate size
  (rounded to the age STREAM chunk boundary, ~64 KiB), and its modification
  time. Encryption hides content and structure, not the fact of the file.
- **That it's an age-encrypted file.** The first line of the file is the
  literal, public spec string `age-encryption.org/v1` — this is how `kshrk`
  itself detects an encrypted archive and prompts for a key. It is not a
  secret and isn't intended to be.
- **A compromised host while the archive is open.** `kshrk ui`/`kshrk open`
  decrypt chunks on demand into process memory as they're browsed (see
  [archive-format.md](archive-format.md#encryption)); a compromised machine
  with the process running, or an attacker who captures the passphrase as
  it's typed, defeats the encryption regardless of the archive itself. This
  is the same trust boundary as any local decryption tool.
- **Recipient revocation.** age has no built-in mechanism to revoke a
  recipient key. If a private key or passphrase may have been exposed, the
  only remedy is to re-encrypt to a new key/recipient set (see
  [Rotating keys](#rotating-keys-today) below) — there is no way to
  retroactively deny that key access to copies already handed out.
- **Old `kshrk` binaries.** A `kshrk` build from before encryption support
  (anything prior to this feature) will fail with a raw, unhelpful
  `zip: not a valid zip file`-style error on an encrypted archive rather than
  a clear "this archive is encrypted" message — there's no way to retrofit
  clearer errors into binaries already in the wild. Current builds detect the
  age header and give a specific error instead.

## Key handling rules

- **Passphrase material never touches the YAML config file or Viper.** The
  `--encrypt-passphrase-file` / `--decrypt-passphrase-file` flags and the
  `$KSHRK_ENCRYPT_PASSPHRASE` / `$KSHRK_DECRYPT_PASSPHRASE` environment
  variables are read directly (`os.Getenv`, file reads) and are never bound
  through Viper — so a passphrase can't accidentally end up committed in
  `config.yaml` or a config file shared alongside a capture. There is
  deliberately no bare `--encrypt-passphrase <string>` flag, since CLI
  arguments are visible in shell history and to other users via `ps`.
- **Treat an age identity (private key) file exactly like an SSH private
  key**: restrict its file permissions (`chmod 600`), never commit it to
  version control, and don't attach it to the same channel you send the
  encrypted archive through.
- **Passphrase mode and recipient mode are mutually exclusive per archive.**
  This is an age library constraint (a passphrase/scrypt recipient must be
  the file's only recipient), not a k8shark design choice — `kshrk` rejects
  the combination before any prompt runs.
- **Prefer recipient (public-key) encryption for anything automated or
  multi-party.** A passphrase is simplest for a single ad-hoc share, but has
  no forward secrecy or per-recipient accountability. Recipient keys let you
  encrypt once to multiple people, and support key rotation without
  re-distributing a shared secret.

## Choosing passphrase vs. recipients

| | Passphrase (`--encrypt`) | Recipients (`--encrypt-recipient`) |
|---|---|---|
| Setup | None — just a shared secret | Recipients generate an age keypair once (`age-keygen`) |
| Sharing model | Whoever has the passphrase can decrypt | Encrypt to specific people's public keys |
| Automation | Passphrase must be distributed to every consumer (file/env) | Encrypt to a public key with no secret material on the encrypting side |
| Rotation | Requires a new shared secret and re-encryption | Add/remove recipients by re-encrypting to a different key set |

## Rotating keys today

There's no dedicated `kshrk encrypt`/`kshrk decrypt` command yet (tracked for
a later milestone). In the meantime, `kshrk redact` can serve as a re-encrypt
tool even with no redaction rules — pass `--decrypt-*` for the source key and
`--encrypt-*` for the new key/recipients:

```sh
kshrk redact --in old.kshrk --out new.kshrk \
  --decrypt-passphrase-file old-pass.txt \
  --encrypt-recipient age1newrecipient...
```

**Caveat**: this always marks the output archive's metadata as `redacted:
true`, even if no redaction actually occurred, since `kshrk redact` doesn't
distinguish "re-key only" from "redact" today. Treat this as a workaround,
not the intended long-term interface.
