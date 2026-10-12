package env

import (
	"log"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// LoadFiles reads .env from the working directory, then ../.env, so services
// run from their own folder pick up the repository-level file. Variables
// already set in the environment are not overwritten.
func LoadFiles() {
	for _, path := range []string{".env", "../.env"} {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if err := godotenv.Load(path); err != nil {
			log.Printf("env file %s: %v", path, err)
			continue
		}
		log.Printf("loaded env file %s", path)
	}
}

func Or(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		log.Printf("%s not set, using default", key)
		return fallback
	}
	return value
}
