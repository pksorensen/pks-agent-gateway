package auth

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/pksorensen/pks-agent-gateway/src/cli/internal/config"
)

// Cred holds the persisted login credentials.
type Cred struct {
	Sub          string `json:"sub"`
	Email        string `json:"email"`
	RefreshToken string `json:"refreshToken"`
}

func credPath() string {
	return filepath.Join(config.ConfigDir(), "cred.json")
}

// LoadCred reads cred.json; returns (nil, nil) if the file does not exist.
func LoadCred() (*Cred, error) {
	data, err := os.ReadFile(credPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var c Cred
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// SaveCred writes cred.json with mode 0600.
func SaveCred(c *Cred) error {
	if err := os.MkdirAll(config.ConfigDir(), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := credPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, credPath())
}

// ClearCred removes cred.json.
func ClearCred() error {
	err := os.Remove(credPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
