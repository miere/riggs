#!/usr/bin/env bash
# Sign a macOS binary with the project's self-signed certificate and prove the
# result still matches the pinned designated requirement.
#
# macOS pins a TCC grant (Full Disk Access, folder access, Accessibility) to the
# binary's designated requirement. The Rust linker emits an ad-hoc signature,
# whose requirement degenerates to `cdhash H"..."`, so every release looks like
# a new program and the user is asked to approve it again. Signing with a real
# certificate anchors the requirement to the certificate's leaf hash instead,
# and the grant survives updates.
#
# Usage: sign-macos.sh <binary>
# Env:   P12_B64 (base64 .p12), P12_PW, SIGN_IDENTIFIER, SIGN_LEAF_SHA1
set -euo pipefail

bin="${1:?usage: sign-macos.sh <binary>}"
: "${P12_B64:?P12_B64 is empty - set the MACOS_CERT_P12_BASE64 secret}"
: "${P12_PW?P12_PW is unset - set the MACOS_CERT_PASSWORD secret}"
: "${SIGN_IDENTIFIER:?}" "${SIGN_LEAF_SHA1:?}"

work="$(mktemp -d)"
keychain="$work/sign.keychain-db"
kc_pw="$(uuidgen)"
trap 'security delete-keychain "$keychain" 2>/dev/null || true; rm -rf "$work"' EXIT

echo "$P12_B64" | base64 --decode > "$work/exported.p12"
# An OpenSSL 3 export uses AES-256 and a SHA-256 MAC, which `security import`
# on macOS 14 rejects as "MAC verification failed". Re-wrap it with 3DES and a
# SHA-1 MAC, which every macOS reads. Through a file, not a pipe: -export reads
# its input twice, once for the key and once for the certificates.
openssl pkcs12 -in "$work/exported.p12" -passin env:P12_PW -nodes -out "$work/pair.pem"
openssl pkcs12 -export -in "$work/pair.pem" -out "$work/cert.p12" -passout env:P12_PW \
  -keypbe PBE-SHA1-3DES -certpbe PBE-SHA1-3DES -macalg sha1
security create-keychain -p "$kc_pw" "$keychain"
security unlock-keychain -p "$kc_pw" "$keychain"
security import "$work/cert.p12" -k "$keychain" -P "$P12_PW" -T /usr/bin/codesign
security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$kc_pw" "$keychain" > /dev/null

# On the CI runners codesign ignores --keychain unless that keychain is also on
# the user search list, and fails with "item could not be found".
security list-keychains -d user -s "$keychain" $(security list-keychains -d user | tr -d '"')

# The leaf hashes on offer, so a mismatch with SIGN_LEAF_SHA1 shows in the log.
security find-identity "$keychain"

# Sign by leaf hash rather than by name. codesign accepts an untrusted
# self-signed identity when it is named by hash, so no trust settings change.
codesign --force --keychain "$keychain" --sign "$SIGN_LEAF_SHA1" \
  --identifier "$SIGN_IDENTIFIER" "$bin"
codesign -d -r- "$bin" 2>&1 | tail -1

# The same evaluation TCC performs against the requirement stored alongside a
# grant, so passing it means existing grants still match this build.
expected="identifier \"$SIGN_IDENTIFIER\" and certificate leaf = H\"$SIGN_LEAF_SHA1\""
codesign --verify -R "=$expected" "$bin" || {
  echo "::error::$bin does not satisfy '$expected' - releasing it would break every existing grant"
  exit 1
}
echo "OK - existing TCC grants survive this build."
