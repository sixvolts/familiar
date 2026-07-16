package actions

// Embed the IANA timezone database in any binary that links this package
// (the gateway) so scheduled-action timezones resolve on hosts without a
// system zoneinfo database — containers and stripped-down deploy boxes.
//
// Without it, time.LoadLocation("America/Chicago") fails at runtime, which
// makes Validate reject every non-UTC action and the runner fall back to
// UTC. Keeping the import next to the code that calls LoadLocation (rather
// than in package main) means the actions test binary embeds it too, so
// timezone tests exercise the same data the gateway ships.
import _ "time/tzdata"
