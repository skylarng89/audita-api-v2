package main

import (
	"errors"
	"os"
)

// errNatsDisconnected marks a failed NATS readiness check.
var errNatsDisconnected = errors.New("nats: disconnected")

func getEnvOrDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
