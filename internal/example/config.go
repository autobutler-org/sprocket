package example

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

const (
	EnvPrefix = "TGO"
)

type Mode string

const (
	ModeWarn    Mode = "warn"
	ModeEnforce Mode = "enforce"
)

//go:embed default_config.yaml
var defaultConfig []byte

type Config struct {
	Port  int `mapstructure:"port"`
	HTTPS struct {
		Enabled bool `mapstructure:"enabled"`
	} `mapstructure:"https"`
}

func (c Config) Defaults() io.Reader {
	return bytes.NewReader(defaultConfig)
}

type Defaulter interface {
	// A Reader used for reading the default values for the config in YAML format.
	Defaults() io.Reader
}

func forbidUnusedKeys(c *mapstructure.DecoderConfig) {
	c.ErrorUnused = true
}

// Loads the default config, making sure the all fields defined
// in the Config struct are bound by viper and defaults are loaded.
func loadDefaultConfig(config interface{}, defaults io.Reader) error {
	// See https://github.com/spf13/viper/issues/584
	cfgMap := make(map[string]interface{})
	if err := mapstructure.Decode(config, &cfgMap); err != nil {
		return err
	}

	cfgJSONBytes, err := json.Marshal(&cfgMap)
	if err != nil {
		return err
	}

	viper.SetConfigType("json")
	if err = viper.ReadConfig(bytes.NewReader(cfgJSONBytes)); err != nil {
		return err
	}
	if err = viper.Unmarshal(config, forbidUnusedKeys); err != nil {
		return err
	}

	// the real deal: read the default YAML
	viper.SetConfigType("yaml")
	if err = viper.ReadConfig(defaults); err != nil {
		return err
	}
	if err = viper.Unmarshal(config, forbidUnusedKeys); err != nil {
		return err
	}

	return nil
}

func LoadConfig(config Defaulter) error {
	viper.SetEnvPrefix(EnvPrefix)
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	// Set up Viper with environment variable configuration
	viper.AutomaticEnv()

	if err := loadDefaultConfig(config, config.Defaults()); err != nil {
		return fmt.Errorf("failed to load default config: %w", err)
	}

	configPath := viper.GetString("CONFIG_PATH")
	viper.SetConfigType("yaml")
	viper.SetConfigFile(configPath)

	if configPath != "" {
		if err := viper.ReadInConfig(); err != nil {
			return fmt.Errorf("unable to read config, %w", err)
		}
		if err := viper.Unmarshal(config, forbidUnusedKeys); err != nil {
			return fmt.Errorf("unable to decode config into struct, %w", err)
		}
	}

	return nil
}
