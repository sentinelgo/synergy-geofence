package main

import (
	_ "time/tzdata"

	"github.com/sentinelgo/synergy-geofence/cmd"
)

func main() {
	cmd.Execute()
}
