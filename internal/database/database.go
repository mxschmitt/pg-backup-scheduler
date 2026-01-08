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
	// Validate connection URL
	_, err := url.Parse(connectionURL)
	if err != nil {
		return nil, fmt.Errorf("invalid connection URL: %w", err)
	}

	identifier := strings.ToLower(projectName)

	return &Database{
		ConnectionURL: connectionURL,
		Identifier:    identifier,
	}, nil
}
