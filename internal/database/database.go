package database

import (
	"fmt"
	"net/url"
	"strings"
)

type Database struct {
	ConnectionURL string
	Identifier    string
}

func New(connectionURL, projectName string) (*Database, error) {
	parsed, err := url.Parse(connectionURL)
	if err != nil {
		return nil, fmt.Errorf("invalid connection URL: %w", err)
	}

	host := parsed.Hostname()
	if host == "" {
		host = "unknown"
	}

	dbName := strings.TrimPrefix(parsed.Path, "/")
	if dbName == "" {
		dbName = "postgres"
	}

	identifier := strings.ToLower(projectName)

	return &Database{
		ConnectionURL: connectionURL,
		Identifier:    identifier,
	}, nil
}
