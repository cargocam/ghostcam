package sensors

import "testing"

func TestParseCellLocation(t *testing.T) {
	// Representative `mmcli -m 0 --location-get` 3GPP section (LTE: both
	// LAC and TAC present).
	out := `  -------------------------
  3GPP location |   operator code: 310410
                | location area code: 6647
                | tracking area code: 12345
                |         cell id: 0A1B2C3D
  -------------------------
`
	c := parseCellLocation(out)
	if c.Operator != "310410" {
		t.Errorf("Operator = %q want 310410", c.Operator)
	}
	if c.LAC != "6647" {
		t.Errorf("LAC = %q want 6647", c.LAC)
	}
	if c.TAC != "12345" {
		t.Errorf("TAC = %q want 12345", c.TAC)
	}
	if c.CID != "0A1B2C3D" {
		t.Errorf("CID = %q want 0A1B2C3D", c.CID)
	}
}

func TestParseCellLocationEmpty(t *testing.T) {
	// mmcli prints "--" when 3GPP location has no value; nothing should match.
	out := `  3GPP location |   operator code: --
                | location area code: --
                |         cell id: --
`
	c := parseCellLocation(out)
	if c.Operator != "" || c.LAC != "" || c.TAC != "" || c.CID != "" {
		t.Errorf("expected all-empty, got %+v", c)
	}
}
