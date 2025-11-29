package main

import (
	"encoding/json"
	"log"
)

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
		return
	}
	// Log config
	cfgBytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Printf("Failed to format config for logging: %v", err)
	} else {
		log.Printf("Loaded config:\n%s", string(cfgBytes))
	}

}
