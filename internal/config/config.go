package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server ServerConfig `yaml:"server"`
	Auth   AuthConfig   `yaml:"auth"`
	Game   GameConfig   `yaml:"game"`
	DB     DBConfig     `yaml:"db"`
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type AuthConfig struct {
	JWTSecret        string `yaml:"jwt_secret"`
	AccessTokenHours int    `yaml:"access_token_hours"`
	CodeTTLMinutes   int    `yaml:"code_ttl_minutes"`
}

type GameConfig struct {
	InitialSeconds int `yaml:"initial_seconds"`
}

type DBConfig struct {
	Path string `yaml:"path"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Auth.JWTSecret == "" {
		cfg.Auth.JWTSecret = "change-me-before-production"
	}
	if cfg.Auth.AccessTokenHours == 0 {
		cfg.Auth.AccessTokenHours = 168
	}
	if cfg.Auth.CodeTTLMinutes == 0 {
		cfg.Auth.CodeTTLMinutes = 10
	}
	if cfg.Game.InitialSeconds == 0 {
		cfg.Game.InitialSeconds = 600
	}
	if cfg.DB.Path == "" {
		cfg.DB.Path = "button-air-drop.db"
	}

	return &cfg, nil
}
