package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/SherClockHolmes/webpush-go"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	vapidSecretName = "vapid"
	vapidPurpose    = "vapid-private-key"
)

// errMalformedKeys is returned for a stored key pair that cannot be used.
var errMalformedKeys = errors.New("stored VAPID keys are malformed")

// VAPIDKeys is the instance's VAPID key pair: the uncompressed P-256 public
// key and the private scalar, both in unpadded base64url.
type VAPIDKeys struct {
	Public, Private string
}

// VAPID manages the VAPID key pair stored in instance_secrets. The pair is
// created when first needed; the private key is sealed with secretbox.
type VAPID struct {
	Store *store.Store
	Box   *secretbox.Box
	Now   func() time.Time
}

type storedVAPID struct {
	Public  string `json:"public"`
	Private string `json:"private"`
}

// Keys returns the key pair, creating it when there is none. When the stored
// private key cannot be decrypted (ASTOVIA_JWT_SECRET changed), a new pair
// replaces it and every push subscription is deleted, because subscriptions
// made for the old public key cannot be used. Concurrent callers agree on one
// pair: creation is an insert that keeps the first row, and replacement only
// succeeds while the old value is still stored; the losers read again.
func (v *VAPID) Keys(ctx context.Context) (VAPIDKeys, error) {
	for attempt := 0; attempt < 3; attempt++ {
		sec, err := v.Store.InstanceSecret(ctx, vapidSecretName)
		if errors.Is(err, store.ErrSecretNotFound) {
			value, _, err := v.generate()
			if err != nil {
				return VAPIDKeys{}, err
			}
			if _, err = v.Store.CreateInstanceSecret(ctx, vapidSecretName, value, v.Now().Unix()); err != nil {
				return VAPIDKeys{}, err
			}
			continue
		}
		if err != nil {
			return VAPIDKeys{}, err
		}
		keys, err := v.parse(sec.Value)
		if err == nil {
			return keys, nil
		}
		if !errors.Is(err, secretbox.ErrUndecryptable) && !errors.Is(err, errMalformedKeys) {
			return VAPIDKeys{}, err
		}
		value, keys, err := v.generate()
		if err != nil {
			return VAPIDKeys{}, err
		}
		replaced, err := v.Store.ReplaceInstanceSecret(ctx, vapidSecretName, sec.Value, value, v.Now().Unix(), true)
		if err != nil {
			return VAPIDKeys{}, err
		}
		if replaced {
			log.Print("web push: the VAPID private key could not be decrypted; created a new key pair and removed every push subscription")
			return keys, nil
		}
	}
	return VAPIDKeys{}, errors.New("web push: VAPID keys are unavailable")
}

func (v *VAPID) generate() (string, VAPIDKeys, error) {
	private, public, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", VAPIDKeys{}, err
	}
	sealed, err := v.Box.Seal(vapidPurpose, private)
	if err != nil {
		return "", VAPIDKeys{}, err
	}
	b, err := json.Marshal(storedVAPID{Public: public, Private: sealed})
	if err != nil {
		return "", VAPIDKeys{}, err
	}
	return string(b), VAPIDKeys{Public: public, Private: private}, nil
}

func (v *VAPID) parse(value string) (VAPIDKeys, error) {
	var s storedVAPID
	if err := json.Unmarshal([]byte(value), &s); err != nil {
		return VAPIDKeys{}, errMalformedKeys
	}
	if pub, err := base64.RawURLEncoding.DecodeString(s.Public); err != nil || len(pub) != 65 || pub[0] != 4 {
		return VAPIDKeys{}, errMalformedKeys
	}
	private, err := v.Box.Open(vapidPurpose, s.Private)
	if err != nil {
		return VAPIDKeys{}, err
	}
	if b, err := base64.RawURLEncoding.DecodeString(private); err != nil || len(b) != 32 {
		return VAPIDKeys{}, errMalformedKeys
	}
	return VAPIDKeys{Public: s.Public, Private: private}, nil
}
