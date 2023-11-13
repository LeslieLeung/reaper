package config

import (
	"github.com/leslieleung/reaper/internal/typedef"
	"github.com/leslieleung/reaper/internal/ui"
	"github.com/spf13/viper"
)

type Config struct {
	Repository  []typedef.Repository   `yaml:"repository"`
	Storage     []typedef.MultiStorage `yaml:"storage"`
	GitHubToken string                 `yaml:"githubToken"`
}

var Path string

var vp *viper.Viper
var ins *Config

func Init() {
	vp = viper.New()
	vp.SetConfigFile(Path)
	err := vp.ReadInConfig()
	if err != nil {
		ui.ErrorfExit("Error reading config file, %s", err)
	}
	err = vp.Unmarshal(&ins)
	if err != nil {
		ui.ErrorfExit("Error unmarshalling config file, %s", err)
	}
}

func GetIns() *Config {
	return ins
}

func GetStorageMap() map[string]typedef.MultiStorage {
	storageMap := make(map[string]typedef.MultiStorage)
	for _, storage := range ins.Storage {
		storageMap[storage.Name] = storage
	}
	return storageMap
}
