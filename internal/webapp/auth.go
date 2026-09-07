package webapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// validateInitData authenticates a Telegram Mini App initData string per
// https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app
// and returns the Telegram user id (== private chat id) as a string.
func validateInitData(initData, botToken string, maxAge time.Duration) (string, error) {
	vals, err := url.ParseQuery(initData)
	if err != nil {
		return "", fmt.Errorf("bad initData: %w", err)
	}
	gotHash := vals.Get("hash")
	if gotHash == "" {
		return "", fmt.Errorf("initData has no hash")
	}

	// data_check_string: all fields except hash, sorted, "key=value" joined by \n.
	keys := make([]string, 0, len(vals))
	for k := range vals {
		if k != "hash" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+vals.Get(k))
	}
	dataCheck := strings.Join(lines, "\n")

	secret := hmacSHA256([]byte(botToken), []byte("WebAppData"))
	want := hex.EncodeToString(hmacSHA256([]byte(dataCheck), secret))
	if !hmac.Equal([]byte(want), []byte(gotHash)) {
		return "", fmt.Errorf("initData hash mismatch")
	}

	if maxAge > 0 {
		ts, err := strconv.ParseInt(vals.Get("auth_date"), 10, 64)
		if err != nil {
			return "", fmt.Errorf("initData has no auth_date")
		}
		if time.Since(time.Unix(ts, 0)) > maxAge {
			return "", fmt.Errorf("initData expired")
		}
	}

	var user struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(vals.Get("user")), &user); err != nil || user.ID == 0 {
		return "", fmt.Errorf("initData has no user")
	}
	return strconv.FormatInt(user.ID, 10), nil
}

func hmacSHA256(data, key []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}
