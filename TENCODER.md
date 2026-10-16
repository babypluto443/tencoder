# Tencoder 项目

## 技术栈
- Go 1.25+

## 代码规范
- commit message 用英文
- 变量命名用 camelCase（Go 惯例，与 gofmt / golint 一致）
- 导出符号需要注释，注释以标识符名开头
- 新增代码必须有对应测试，核心并发路径跑 `go test -race`
