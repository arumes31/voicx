# Client update signing

VoicX release clients accept an update only when `checksums.txt` has a valid
Ed25519 signature from an embedded trusted key and its signed version matches
the GitHub release tag. Update metadata, manifests, signatures, and binaries
also have explicit download limits. Non-loopback update URLs must use HTTPS.

## Initial setup

Generate the key on a secured operator workstation, outside the repository:

```powershell
go run ./cmd/signrelease -generate-key C:\secure\voicx-update-signing-key.txt
```

The command creates the private-key file exclusively with restrictive process
permissions and prints only the corresponding public key. Move the private key
into the organization secret manager, then securely remove the temporary file.
Never commit it, attach it to a release, or paste it into logs or tickets.

Configure the repository with:

- Actions secret `VOICX_UPDATE_SIGNING_KEY`: the single base64 line stored in
  the generated private-key file;
- Actions variable `VOICX_UPDATE_PUBLIC_KEYS`: the printed base64 public key.

Tagged release builds validate the public-key configuration before building so
the client never embeds an empty trust root. The private key is first consumed
after the build, by the isolated manifest-signing step; it is not available to
the build or publishing jobs. The public key is embedded in the Windows client
through `internal/version.UpdatePublicKeys`.

## Rotation

`VOICX_UPDATE_PUBLIC_KEYS` accepts comma-separated public keys. Rotate without
stranding installed clients in three releases:

1. Generate a new key and publish a release embedding `old,new`, signed by the
   old private key.
2. After that client is deployed, change `VOICX_UPDATE_SIGNING_KEY` to the new
   private key and keep publishing clients that trust `old,new`.
3. After the supported upgrade window, publish a client that embeds only `new`.

If a private key may be compromised, stop releases, remove it from the signing
secret, retain only uncompromised public keys in future builds, and distribute a
trusted recovery build out of band. A client that trusts only the compromised
key cannot safely bootstrap a replacement key through the compromised channel.

## Release artifacts

The release workflow publishes:

- `voicx-client-windows-amd64.exe`;
- `voicx-server-linux-amd64`;
- `checksums.txt`, beginning with `# voicx-version: <tag>`;
- `checksums.txt.sig`, a base64 detached Ed25519 signature over the exact bytes
  of `checksums.txt`.

Each binary receives a GitHub Actions build-provenance attestation after it is
built. The signing job creates the manifest without the private key, invokes a
prebuilt signer in the only secret-bearing step, then immediately verifies the
signature and both declared checksums with the configured public key. After the
signed artifact crosses into the publishing job, that signature-and-checksum
verification is repeated immediately before the release upload.

Both signing and verification require an explicit `-version <tag>`. Before
creating a signature, the signer uses the shared manifest validator to require
exactly one non-empty `# voicx-version: <tag>` header matching that value. It
creates `checksums.txt.sig` exclusively with mode `0600`: an existing file or
symlink is refused rather than overwritten, and a newly created partial output
is removed if writing, syncing, or closing fails.

The signature authenticates both binaries and the release-version binding. It
does not protect a workstation where the signing key or build environment is
already compromised; provenance and hardened release runners remain separate
supply-chain controls.
