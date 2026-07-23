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

// CellLocation is the serving-cell 3GPP identifiers from
// `mmcli -m 0 --location-get`. All fields are "" when the corresponding
// line is missing (3GPP location disabled, no modem, etc.).
type CellLocation struct {
	Operator string // MCC+MNC, e.g. "310410"
	LAC      string // location area code (2G/3G)
	TAC      string // tracking area code (LTE)
	CID      string // cell id (often hex)
}

var (
	// mmcli's `--location-get` 3GPP block prints the operator as separate
	// `operator mcc:` / `operator mnc:` lines (verified on SIM7600G-H,
	// ModemManager 1.x); older/other builds print a combined
	// `operator code:` line. Handle both — the operator string we produce
	// is MCC+MNC concatenated, which the server splits back apart.
	cellOpRE  = regexp.MustCompile(`(?i)operator code\s*:\s*([0-9]+)`)
	cellMCCRE = regexp.MustCompile(`(?i)operator mcc\s*:\s*([0-9]+)`)
	cellMNCRE = regexp.MustCompile(`(?i)operator mnc\s*:\s*([0-9]+)`)
	cellLACRE = regexp.MustCompile(`(?i)location area code\s*:\s*([0-9A-Fa-f]+)`)
	cellTACRE = regexp.MustCompile(`(?i)tracking area code\s*:\s*([0-9A-Fa-f]+)`)
	cellCIDRE = regexp.MustCompile(`(?i)cell id\s*:\s*([0-9A-Fa-f]+)`)

	modemPathRE = regexp.MustCompile(`/Modem/(\d+)`)
)

// parseModemIndex extracts the first modem index from `mmcli -L` output
// (e.g. ".../Modem/2" → "2"), falling back to "0" when nothing matches.
// The SIM7600 re-enumerates with a NEW index after a reset/brownout, so
// the daemon must resolve the index at read time rather than hardcoding
// `-m 0` — otherwise a single modem reset silently blanks modem_rat and
// the cell/coarse-location fields even though the modem is alive.
func parseModemIndex(mmcliListOut string) string {
	if m := modemPathRE.FindStringSubmatch(mmcliListOut); len(m) == 2 {
		return m[1]
	}
	return "0"
}

// parseCellLocation is the pure-text half of the cell-location reader.
// mmcli prints "--" for fields it has no value for; the regexes only
// match real digits/hex, so those come back "".
func parseCellLocation(out string) CellLocation {
	c := CellLocation{}
	if m := cellOpRE.FindStringSubmatch(out); len(m) == 2 {
		c.Operator = m[1]
	} else {
		var mcc, mnc string
		if m := cellMCCRE.FindStringSubmatch(out); len(m) == 2 {
			mcc = m[1]
		}
		if m := cellMNCRE.FindStringSubmatch(out); len(m) == 2 {
			mnc = m[1]
		}
		if mcc != "" && mnc != "" {
			c.Operator = mcc + mnc
		}
	}
	if m := cellLACRE.FindStringSubmatch(out); len(m) == 2 {
		c.LAC = m[1]
	}
	if m := cellTACRE.FindStringSubmatch(out); len(m) == 2 {
		c.TAC = m[1]
	}
	if m := cellCIDRE.FindStringSubmatch(out); len(m) == 2 {
		c.CID = m[1]
	}
	return c
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
