
# windows
```
  步骤 1：编译后端

  在 PowerShell 或 Git Bash 中执行：

  # 进入项目目录
  cd D:\Project\go\picoclaw

  # 下载依赖
  go mod download

  # 运行 go generate
  go generate ./...

  # 编译主程序
  $env:CGO_ENABLED=0; go build -v -tags stdjson -ldflags "-s -w" -o build/picoclaw.exe ./cmd/picoclaw

  ---
  步骤 2：编译前端并构建 Launcher

  # 进入前端目录，安装依赖并构建
  cd D:\Project\go\picoclaw\web\frontend
  pnpm install
  pnpm build:backend

  # 返回项目根目录
  cd D:\Project\go\picoclaw

  # 编译 Launcher（Web 控制台）
  $env:CGO_ENABLED=0; go build -v -tags stdjson -ldflags "-s -w" -o build/picoclaw-launcher.exe ./web/backend

  ---
  步骤 3：初始化配置

  # 运行 onboard 初始化
  .\build\picoclaw.exe onboard

  这会在 %USERPROFILE%\.picoclaw\ 创建配置文件。

  ---
  步骤 4：配置 API 密钥

  编辑配置文件，添加你的 API 密钥：

  notepad $env:USERPROFILE\.picoclaw\config.json

  在 providers 部分添加你的 API key。

  ---
  步骤 5：启动服务

  启动 Launcher（前端 + 后端）：

  .\build\picoclaw-launcher.exe -public -no-browser

  然后访问：
  - 前端 Web 控制台：http://localhost:18800
  - 后端健康检查：http://localhost:18790/health

  或只启动 Gateway（后端）：

  .\build\picoclaw.exe gateway

  ---
  一键脚本（可选）

  如果你想更方便，我可以帮你创建一个 PowerShell 脚本 build.ps1：

  # build.ps1 - PicoClaw Windows 构建脚本

  Write-Host "=== PicoClaw Windows Build ===" -ForegroundColor Cyan

  # 1. 下载依赖
  Write-Host "[1/4] 下载 Go 依赖..." -ForegroundColor Yellow
  go mod download

  # 2. 生成代码
  Write-Host "[2/4] 运行 go generate..." -ForegroundColor Yellow
  go generate ./...

  # 3. 编译后端
  Write-Host "[3/4] 编译 picoclaw.exe..." -ForegroundColor Yellow
  $env:CGO_ENABLED = "0"
  go build -v -tags stdjson -ldflags "-s -w" -o build/picoclaw.exe ./cmd/picoclaw

  # 4. 构建前端并编译 Launcher
  Write-Host "[4/4] 构建前端并编译 picoclaw-launcher.exe..." -ForegroundColor Yellow
  Push-Location web/frontend
  pnpm install
  pnpm build:backend
  Pop-Location
  go build -v -tags stdjson -ldflags "-s -w" -o build/picoclaw-launcher.exe ./web/backend

  Write-Host "=== 构建完成 ===" -ForegroundColor Green
  Write-Host "可执行文件位于: build\" -ForegroundColor Green
  Write-Host ""
  Write-Host "启动命令:" -ForegroundColor Cyan
  Write-Host "  .\build\picoclaw-launcher.exe -public -no-browser   # 启动 Web 控制台"
  Write-Host "  .\build\picoclaw.exe gateway                        # 只启动后端"

  ---
  常见问题

  ┌──────────────────┬──────────────────────────────────────────────────────────┐
  │       问题       │                         解决方案                         │
  ├──────────────────┼──────────────────────────────────────────────────────────┤
  │ go generate 报错 │ 确保在项目根目录执行                                     │
  ├──────────────────┼──────────────────────────────────────────────────────────┤
  │ 前端构建失败     │ 确保 pnpm 已安装：npm install -g pnpm                    │
  ├──────────────────┼──────────────────────────────────────────────────────────┤
  │ 端口被占用       │ 检查 18800 和 18790 端口：netstat -ano | findstr "18800" │
  └──────────────────┴──────────────────────────────────────────────────────────┘

```