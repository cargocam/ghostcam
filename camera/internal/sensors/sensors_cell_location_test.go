package sensors

import "testing"

func TestParseCellLocation(t *testing.T) {
	// The ACTUAL `mmcli -m 0 --location-get` 3GPP block on the SIM7600G-H
	// (ModemManager 1.x): operator is split into mcc/mnc lines, not a
	// combined "operator code" line. Verified from a field diag bundle.
	out := `  --------------------------
  3GPP |       operator mcc: 310
       |       operator mnc: 410
       | location area code: 0000
       | tracking area code: 008308
       |            cell id: 04BCFDB7
  --------------------------
`
	c := parseCellLocation(out)
	if c.Operator != "310410" {
		t.Errorf("Operator = %q want 310410 (mcc+mnc)", c.Operator)
	}
	if c.LAC != "0000" {
		t.Errorf("LAC = %q want 0000", c.LAC)
	}
	if c.TAC != "008308" {
		t.Errorf("TAC = %q want 008308", c.TAC)
	}
	if c.CID != "04BCFDB7" {
		t.Errorf("CID = %q want 04BCFDB7", c.CID)
	}
}

func TestParseCellLocationCombinedOperator(t *testing.T) {
	// Older/other mmcli builds print a combined "operator code" line — keep
	// supporting it.
	out := "3GPP | operator code: 310410\n | cell id: 1234\n"
	c := parseCellLocation(out)
	if c.Operator != "310410" {
		t.Errorf("Operator = %q want 310410", c.Operator)
	}
}

func TestParseCellLocationEmpty(t *testing.T) {
	out := `  3GPP |       operator mcc: --
       |       operator mnc: --
       |            cell id: --
`
	c := parseCellLocation(out)
	if c.Operator != "" || c.CID != "" {
		t.Errorf("expected empty, got %+v", c)
	}
}

func TestParseModemIndex(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"    /org/freedesktop/ModemManager1/Modem/0 [QUALCOMM] SIM7600", "0"},
		{"    /org/freedesktop/ModemManager1/Modem/2 [QUALCOMM] SIM7600", "2"},
		{"    /org/freedesktop/ModemManager1/Modem/13 [x] y", "13"},
		{"No modems were found", "0"}, // fallback
		{"", "0"},
	}
	for _, c := range cases {
		if got := parseModemIndex(c.in); got != c.want {
			t.Errorf("parseModemIndex(%q) = %q want %q", c.in, got, c.want)
		}
	}
}
