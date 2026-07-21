package network

import "strings"

// cellularConnName is the NetworkManager connection the daemon creates
// for the cellular data bearer. Stable so re-runs find and reuse it.
const cellularConnName = "ghostcam-cellular"

// cellularCredArgs builds the optional gsm.username/gsm.password nmcli
// arguments for APNs that require PAP/CHAP auth. Empty creds add nothing.
func cellularCredArgs(user, pass string) []string {
	var args []string
	if user != "" {
		args = append(args, "gsm.username", user)
	}
	if pass != "" {
		args = append(args, "gsm.password", pass)
	}
	return args
}

// scanCellularConns parses `nmcli -t -f NAME,TYPE connection show` output.
// hasGSM is true if any gsm-type connection exists; hasOurs is true if the
// named connection (conName) is present. Pure so it unit-tests on any host.
func scanCellularConns(nmcliOut, conName string) (hasGSM, hasOurs bool) {
	for _, line := range strings.Split(nmcliOut, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// -t output is colon-separated NAME:TYPE. A NAME could in theory
		// contain an escaped colon; TYPE is the last field regardless, so
		// split on the last colon.
		idx := strings.LastIndex(line, ":")
		if idx < 0 {
			continue
		}
		name, typ := line[:idx], line[idx+1:]
		if typ == "gsm" {
			hasGSM = true
		}
		if name == conName {
			hasOurs = true
		}
	}
	return hasGSM, hasOurs
}
