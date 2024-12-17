package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	Port           int    `json:"port"`
	WorkingDir     string `json:"working_dir"`
	ConfigFilePath string `json:"config_file_path"`
	Db             struct {
		Location string `json:"location"`
	} `json:"db"`
	Media struct {
		Location string `json:"location"`
	} `json:"media"`
}

var conf Config

const (
	defaultPort       = 3000
	defaultWorkingDir = "CTP"
	defaultConfigDir  = "config"
	defaultConfig     = "config.conf"
	defaultDb         = "db"
	defaultMediaDir   = "media"
)

func init() {
	homePath := os.Getenv("HOME")
	if homePath == "" {
		homePath = os.Getenv("USERPROFILE")
	}

	conf.ConfigFilePath = os.Getenv("CONFIG_PATH")
	if conf.ConfigFilePath == "" {
		conf.ConfigFilePath = filepath.Join(homePath, defaultWorkingDir, defaultConfigDir, defaultConfig)
	}

	conf.WorkingDir = os.Getenv("WORKING_DIR")
	if conf.WorkingDir == "" {
		conf.WorkingDir = filepath.Join(homePath, defaultWorkingDir)
	}

	conf.Db.Location = os.Getenv("DB_PATH")
	if conf.Db.Location == "" {
		conf.Db.Location = filepath.Join(conf.WorkingDir, defaultConfigDir, defaultDb)
	}
	conf.Media.Location = os.Getenv("MEDIA_DIR")
	if conf.Media.Location == "" {
		conf.Media.Location = filepath.Join(conf.WorkingDir, defaultMediaDir)
	}
	conf.Port, _ = strconv.Atoi(os.Getenv("PORT"))
	if conf.Port == 0 {
		conf.Port = defaultPort
	}
}

// Load configuration from the config file
func LoadConfig() {
	// Read config file if it exists
	file, err := os.Open(conf.ConfigFilePath)
	if err == nil {
		defer file.Close()
		decoder := json.NewDecoder(file)
		err = decoder.Decode(&conf)
		if err != nil {
			println("Error reading config file:", err)
		}
	} else {
		// Create the config file with default values if it doesn't exist
		SaveConfig()
	}
}

// Save configuration to the config file
func SaveConfig() {
	file, err := os.Create(conf.ConfigFilePath)
	if err != nil {
		println("Error creating config file:", err)
		return
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	err = encoder.Encode(conf)
	if err != nil {
		println("Error writing to config file:", err)
	}
}

// Port returns the port value from the configuration
func Port() int {
	return conf.Port
}

// WorkingDir returns the working directory from the configuration
func WorkingDir() string {
	return conf.WorkingDir
}

// DbLocation returns the database location from the configuration
func DbLocation() string {
	return conf.Db.Location
}

// MediaLocation returns the media location from the configuration
func MediaLocation() string {
	return conf.Media.Location
}
