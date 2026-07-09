package main

import (
	"regexp"
	"strconv"
)

var bodyDenylist = regexp.MustCompile(`(?i)(insufficient credits?|rate limit|exceeded|payment required|unauthorized|forbidden)`)

// shouldRotate returns true and a short reason if the response signals a
// key-level rejection worth rotating on.
func shouldRotate(status int, body []byte) (bool, string) {
	switch status {
	case 402, 429, 401, 403:
		return true, "status " + strconv.Itoa(status)
	}
	if m := bodyDenylist.Find(body); m != nil {
		return true, "body:" + string(m)
	}
	return false, ""
}
