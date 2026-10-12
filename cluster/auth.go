package cluster

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader is the header requests between nodes, and their
// responses, are signed in: when it was signed (in Unix seconds), and an
// HMAC-SHA256 of that, the method, the URI and the body, with the
// cluster's secret.  (Responses are signed with the method "RESPONSE",
// and the request's signature for the URI.)
const SignatureHeader = "X-Honey-Cluster-Signature"

// maxSkew is how far a signature's time may be from now: how far apart
// nodes' clocks may be, and how long a request could be replayed for
// (which does no harm: a purge is only applied once)
const maxSkew = time.Minute

func signature(secret string, t int64, method, uri string, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(t, 10) + "\n" + method + "\n" + uri + "\n"))
	mac.Write(body)
	return mac.Sum(nil)
}

// sign returns the SignatureHeader for a request (or response)
func sign(secret, method, uri string, body []byte, now time.Time) string {
	t := now.Unix()
	return strconv.FormatInt(t, 10) + " " + hex.EncodeToString(signature(secret, t, method, uri, body))
}

// verify returns whether header is a signature of a request (or response)
// made recently with secret
func verify(secret, header, method, uri string, body []byte, now time.Time) bool {
	ts, sig, ok := strings.Cut(header, " ")
	if !ok {
		return false
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if d := now.Sub(time.Unix(t, 0)); d > maxSkew || d < -maxSkew {
		return false
	}
	mac, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	return hmac.Equal(mac, signature(secret, t, method, uri, body))
}
