package sensors

import (
	"regexp"
	"strconv"
	"strings"
)

// ModemSample is what we extract from a single `mmcli -m 0` invocation.
// Both fields are zero-valued ("" / 0) when the corresponding mmcli
// line was missing or unparseable; the caller decides how to surface
// that (typically as a nil pointer in the telemetry envelope).
type ModemSample struct {
	RAT    string // "LTE", "5G_NSA", "WCDMA", "GSM", "" if unknown
	SigPct uint8  // 0-100, 0 if unknown
}

// mmcliAccessTechRE matches the `access tech:` line in `mmcli -m 0`.
// mmcli prints lowercase short tokens (e.g. "lte", "5gnr", "umts");
// we normalize to the upper-case shorthand the UI expects.
var mmcliAccessTechRE = regexp.MustCompile(`(?i)access tech\s*:\s*([^\n]+)`)

// mmcliSigQualityRE matches `signal quality: NN%` allowing trailing
// suffixes mmcli sometimes appends like " (recent)" or " (cached)".
var mmcliSigQualityRE = regexp.MustCompile(`(?i)signal quality\s*:\s*(\d{1,3})\s*%`)

// parseMmcliOutput is the pure-text half of the modem reader. Split
// out for testability — the real reader in sensors_modem_linux.go
// shells out to mmcli, then calls this. Returns a zero ModemSample
// when neither line is present.
func parseMmcliOutput(out string) ModemSample {
	s := ModemSample{}
	if m := mmcliAccessTechRE.FindStringSubmatch(out); len(m) == 2 {
		s.RAT = normalizeRAT(m[1])
	}
	if m := mmcliSigQualityRE.FindStringSubmatch(out); len(m) == 2 {
		if v, err := strconv.Atoi(m[1]); err == nil && v >= 0 && v <= 100 {
			s.SigPct = uint8(v)
		}
	}
	return s
}

// normalizeRAT folds the family of tokens mmcli emits across versions
// into the shorthand the UI / dashboard already use. Unknown values
// pass through uppercased so a new RAT (e.g. a future "6g") still
// surfaces, just unprettified.
func normalizeRAT(raw string) string {
	t := strings.TrimSpace(raw)
	// mmcli can list multiple RATs comma-separated in some outputs
	// (e.g. "lte, 5gnr" during 5G-NSA dual-connect). Take the
	// highest-tier one rather than the first — that's what the
	// camera is actually leaning on.
	parts := strings.Split(t, ",")
	priority := map[string]int{
		"5GNR": 5, "5G": 5,
		"LTE": 4,
		"UMTS": 3, "WCDMA": 3, "HSPA": 3, "HSPA+": 3,
		"GSM": 2, "EDGE": 2, "GPRS": 2,
	}
	best := ""
	bestRank := -1
	for _, p := range parts {
		token := strings.ToUpper(strings.TrimSpace(p))
		token = strings.ReplaceAll(token, " ", "")
		if token == "" {
			continue
		}
		// Common aliases.
		switch token {
		case "5GNR":
			token = "5G_NSA" // mmcli reports the radio; "5G_NSA" matches what
			// the UI shows. Standalone 5G is rare on these modems; if we ever
			// see it we can split based on a separate `5g-mode` field.
		}
		rank, ok := priority[strings.TrimSuffix(token, "_NSA")]
		if !ok {
			rank = 0
		}
		if rank > bestRank {
			best = token
			bestRank = rank
		}
	}
	return best
}
