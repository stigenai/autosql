package postgres

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"

	"autosql/pkg/bootstrap"
)

const bootstrapExtensionAuthorizationDomain = "autosql.bootstrap-extension-runtime-authorization/v1\x00"

var bootstrapRuntimeAuthorizationKey = func() [32]byte {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		panic("initialize bootstrap runtime authorization: " + err.Error())
	}
	return key
}()

// sealBootstrapExtensionAuthorization creates an opaque capability only after
// planning has consumed explicit legacy or verified-manifest authorization.
// The capability is process-local, omitted from serialization, and bound to
// the exact immutable bootstrap digest.
func sealBootstrapExtensionAuthorization(planDigest, resourceID string) []byte {
	mac := hmac.New(sha256.New, bootstrapRuntimeAuthorizationKey[:])
	mac.Write([]byte(bootstrapExtensionAuthorizationDomain))
	mac.Write([]byte(planDigest))
	mac.Write([]byte{0})
	mac.Write([]byte(resourceID))
	return []byte(base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
}

func hasBootstrapExtensionAuthorization(whole bootstrap.Plan, resourceID string) bool {
	return whole.RuntimeAuthorizationMatches(sealBootstrapExtensionAuthorization(whole.Digest, "*")) || whole.RuntimeAuthorizationMatches(sealBootstrapExtensionAuthorization(whole.Digest, resourceID))
}
