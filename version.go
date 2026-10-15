package main

// appVersion 是运行时显示的版本号。
// 构建时通过 -ldflags "-X main.appVersion=<version>" 注入（如 v1.3.0），
// 未注入时显示 "dev"。
// 各构建入口（desktop/build.sh、CI build.yml、Dockerfile）都会注入。
var appVersion = "dev"
