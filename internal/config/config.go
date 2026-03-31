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
	Kakao  KakaoConfig  `yaml:"kakao"`
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

type KakaoConfig struct {
	RestAPIKey   string `yaml:"rest_api_key"`
	ClientSecret string `yaml:"client_secret"`
	RedirectURI  string `yaml:"redirect_uri"`
	Scope        string `yaml:"scope"`
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
		cfg.Game.InitialSeconds = 1800
	}
	if cfg.DB.Path == "" {
		cfg.DB.Path = "button-air-drop.db"
	}
	if cfg.Kakao.RestAPIKey == "" {
		cfg.Kakao.RestAPIKey = os.Getenv("KAKAO_REST_API_KEY")
	}
	if cfg.Kakao.ClientSecret == "" {
		cfg.Kakao.ClientSecret = os.Getenv("KAKAO_CLIENT_SECRET")
	}
	if cfg.Kakao.RedirectURI == "" {
		cfg.Kakao.RedirectURI = os.Getenv("KAKAO_REDIRECT_URI")
	}

	return &cfg, nil
}
