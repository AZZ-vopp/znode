package panel

import "testing"

func TestAggregateUserTrafficWithMultipleUUIDsSharingUID(t *testing.T) {
	got := aggregateUserTraffic([]UserTraffic{
		{UID: 7, Upload: 100, Download: 200},
		{UID: 7, Upload: 30, Download: 40},
		{UID: 8, Upload: 5, Download: 6},
	})

	if got[7][0] != 130 || got[7][1] != 240 {
		t.Fatalf("shared UID traffic was overwritten instead of aggregated: %#v", got[7])
	}
	if got[8][0] != 5 || got[8][1] != 6 {
		t.Fatalf("independent UID traffic changed unexpectedly: %#v", got[8])
	}
}
