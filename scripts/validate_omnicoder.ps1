# OmniCoder-ICX validation runner. Usage: powershell -File scripts\validate_omnicoder.ps1
$ErrorActionPreference = "Continue"
$fail = 0
function Step($name, $cmd) {
  Write-Host "`n=== $name ===" -ForegroundColor Cyan
  Invoke-Expression $cmd
  if ($LASTEXITCODE -ne 0) { Write-Host "FAILED: $name" -ForegroundColor Red; $script:fail++ } else { Write-Host "OK: $name" -ForegroundColor Green }
}
Step "Build"                  "go build ./..."
Step "Vet"                    "go vet ./..."
Step "Unit tests (all)"       "go test -count=1 ./..."
Step "Storage + engine -race" "go test -race -count=1 ./pkg/storage/... ./pkg/engine/..."
Step "Validation suite"       "go test -tags validation -race -v -count=1 -timeout 20m ./pkg/storage/ -run TestValidation"
Step "Fuzz-lite (repeat x5)"  "go test -count=5 ./pkg/storage/titankv/..."
Step "Rebuild binary"         "go build -o omnicoder.exe ./cmd/omnicoder"
Step "CLI vault status"       ".\omnicoder.exe vault status"
Write-Host "`nFailures: $fail" -ForegroundColor $(if ($fail) {"Red"} else {"Green"})
exit $fail
