$env:GOOS = "darwin"
$env:GOARCH = "amd64"
go build -o a2o-mac-amd64 main.go
Write-Host "Built a2o-mac-amd64 (Intel Mac)"

$env:GOARCH = "arm64"
go build -o a2o-mac-arm64 main.go
Write-Host "Built a2o-mac-arm64 (Apple Silicon Mac)"

# Reset environment variables
$env:GOOS = ""
$env:GOARCH = ""
