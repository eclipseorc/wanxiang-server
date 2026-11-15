// Package envfile 解析简单的 KEY=value 配置文件，并将配置装载到进程环境变量。
// 已存在的系统环境变量优先于文件值；开发环境缺少配置文件时允许使用默认配置。
package envfile
