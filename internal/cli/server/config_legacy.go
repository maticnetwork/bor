package server

import (
	"fmt"
	"io/ioutil"

	"github.com/BurntSushi/toml"
)

func readLegacyConfig(path string) (*Config, error) {

	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read toml config file: %v", err)
	}
	tomlData := string(data)

	var conf Config
	if _, err := toml.Decode(tomlData, &conf); err != nil {
		return nil, fmt.Errorf("failed to decode toml config file: %v", err)
	}

	return &conf, nil
}
