package main

import (
	"time"

	// The console renders every timestamp in Tashkent's wall clock, so the
	// zone has to be there whatever the image carries. A scratch container has
	// no zoneinfo, and a console that silently fell back to UTC would show
	// times five hours off the ones the operator is reading on their phone.
	_ "time/tzdata"
)

// standZone is the wall clock the console shows. Everything is stored in UTC —
// timestamps as timestamptz, protocol moments as epoch milliseconds — and only
// the display is local, so a row's meaning never depends on who is reading it.
const standZone = "Asia/Tashkent"

// zone is standZone resolved once. LoadLocation cannot fail here: the zone
// database is embedded above, so a missing zone would be a build that shipped
// without it, which the fallback keeps readable rather than fatal.
var zone = loadZone()

func loadZone() *time.Location {
	loaded, err := time.LoadLocation(standZone)
	if err != nil {
		// Five hours east, which is what Tashkent is all year: Uzbekistan
		// keeps no daylight saving, so a fixed offset is the same clock.
		return time.FixedZone("+05", 5*60*60)
	}
	return loaded
}

// stampFormat is how a moment reads on every screen: sortable, no month names,
// and no timezone suffix, since the whole console is on one clock and the
// header says which.
const stampFormat = "2006-01-02 15:04:05"

// stamp is the SQL that renders a stored timestamp on the stand's clock.
//
// PostgreSQL keeps timestamptz in UTC and formats it in the session's zone, so
// the zone is named in the query rather than left to whatever the connection
// happened to be opened with.
func stamp(column string) string {
	return `to_char(` + column + ` AT TIME ZONE '` + standZone + `', 'YYYY-MM-DD HH24:MI:SS')`
}

// moment renders an epoch-millisecond moment, which is how the protocol carries
// every time it sends. Zero means "not yet", not 1970.
func moment(millis int64) string {
	if millis == 0 {
		return "—"
	}
	return time.UnixMilli(millis).In(zone).Format(stampFormat)
}
