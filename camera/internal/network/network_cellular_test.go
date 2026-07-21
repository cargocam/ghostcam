package network

import (
	"reflect"
	"testing"
)

func TestScanCellularConns(t *testing.T) {
	tests := []struct {
		name        string
		out         string
		wantGSM     bool
		wantOurs    bool
	}{
		{
			name:     "empty (no connections)",
			out:      "",
			wantGSM:  false,
			wantOurs: false,
		},
		{
			name:     "only wifi + ethernet",
			out:      "preconfigured:802-11-wireless\nWired connection 1:802-3-ethernet",
			wantGSM:  false,
			wantOurs: false,
		},
		{
			name:     "our cellular connection present",
			out:      "ghostcam-cellular:gsm\npreconfigured:802-11-wireless",
			wantGSM:  true,
			wantOurs: true,
		},
		{
			name:     "a foreign gsm connection exists (leave alone)",
			out:      "carrier-lte:gsm\nWired connection 1:802-3-ethernet",
			wantGSM:  true,
			wantOurs: false,
		},
		{
			name:     "trailing newline + blank lines tolerated",
			out:      "ghostcam-cellular:gsm\n\n",
			wantGSM:  true,
			wantOurs: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gsm, ours := scanCellularConns(tc.out, cellularConnName)
			if gsm != tc.wantGSM || ours != tc.wantOurs {
				t.Errorf("scanCellularConns() = (gsm=%v, ours=%v), want (gsm=%v, ours=%v)",
					gsm, ours, tc.wantGSM, tc.wantOurs)
			}
		})
	}
}

func TestCellularCredArgs(t *testing.T) {
	tests := []struct {
		user, pass string
		want       []string
	}{
		{"", "", nil},
		{"ghost", "", []string{"gsm.username", "ghost"}},
		{"", "secret", []string{"gsm.password", "secret"}},
		{"ghost", "secret", []string{"gsm.username", "ghost", "gsm.password", "secret"}},
	}
	for _, tc := range tests {
		got := cellularCredArgs(tc.user, tc.pass)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("cellularCredArgs(%q,%q) = %v, want %v", tc.user, tc.pass, got, tc.want)
		}
	}
}
