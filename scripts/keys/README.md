# Release verification keys

This directory contains all keys that are currently signing `pool` releases.

The name of the file must match exactly the suffix that user is going to use
when signing a release. For example, if the key is called `signer.asc` then 
that user must upload a signature file called `manifest-signer-v0.xx.yy-beta.sig`,
otherwise the verification will fail. You can check [lnd's release documentation](https://github.com/lightningnetwork/lnd/blob/master/docs/release.md#signing-an-existing-manifest-file) 
for details on how to create the signature.

In addition to adding the key file here as a .asc file the 
`scripts/verify-install.sh` file must also be updated with the key ID and the
reference to the key file.

# Verifying the Release

In order to verify the release, you'll need to have gpg or gpg2 installed on 
your system. Once you've obtained a copy (and hopefully verified that as well),
you'll first need to import the keys that have signed this release if you 
haven't done so already:

```
curl https://raw.githubusercontent.com/lightningnetwork/lnd/master/scripts/keys/guggero.asc | gpg --import
```

Once you have the required PGP keys, you can verify the release (assuming 
`manifest-guggero-v0.6.1-beta.sig` and `manifest-v0.6.1-beta.txt` are in the 
current directory) with:

```
gpg --verify manifest-guggero-v0.6.1-beta.sig manifest-v0.6.1-beta.txt
```

You should see the following if the verification was successful:

```
gpg: Signature made Di 01 Nov 2022 14:00:20 CET
gpg:                using RSA key F4FC70F07310028424EFC20A8E4256593F177720
gpg: Good signature from "Oliver Gugger <gugger@gmail.com>" [ultimate]
```

That will verify the signature of the manifest file, which ensures integrity 
and authenticity of the archive you've downloaded locally containing the 
binaries. Next, depending on your operating system, you should then re-compute 
the sha256 hash of the archive with `shasum -a 256 <filename>`, compare it with
the corresponding one in the manifest file, and ensure they match exactly.

