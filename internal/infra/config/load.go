package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// Load reads path when it exists; a missing file falls back to the built-in defaults.
func Load(path string) (*Config, error) {
	v := viper.New()
	defaults(v)

	v.SetEnvPrefix("AREDNSCAN")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv()

	if path != "" {
		// Stat is checked directly because with an explicit file set viper surfaces a missing
		// path as *fs.PathError, which no viper error type reports as NotFound.
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			v.SetConfigFile(path)
			v.SetConfigType(strings.ToLower(ext(path)))
			if err := v.ReadInConfig(); err != nil {
				return nil, fmt.Errorf("read config %s: %w", path, err)
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("stat config %s: %w", path, err)
		}
	}

	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	c.Path = path
	c.normalize()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func ext(p string) string {
	// Only the basename may supply the type: "./cfg/config" has a dot but no extension.
	base := p
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		return base[i+1:]
	}
	return "yaml"
}
