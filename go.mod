module codearts2api

go 1.24

// 固定一个已知可用的工具链版本，保证本地与 CI 构建一致。
// 若本机 Go 版本更低，go 命令会自动下载该工具链。
toolchain go1.24.4
