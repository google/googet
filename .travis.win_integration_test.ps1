[CmdletBinding()]
param (
)

# Build googet and Goo Package
Write-Host 'Building googet'
$build_proc = Start-Process go -ArgumentList @('run', 'goopack/goopack.go', './googet.goospec') -Wait -PassThru
if ($build_proc.ExitCode -ne 0) {
  Write-Error "Failed to build goopack got exit code $($build_proc.ExitCode), wanted 0"
  exit $build_proc.ExitCode
}
# Determine output goo pack
$goo = Get-ChildItem .\ | Where-Object {$_.Name -like '*.goo'}
Write-Host "Found built goopack at $($goo.FullName)"

# Setup Environment
$goo_root = $env:ProgramData + '\Googet'
if (!(Test-Path $goo_root)) {
  New-Item -ItemType Directory -Path $goo_root
}

# Install googet
Write-Host "Attempting to use googet to install $($goo.FullName)"
$install_proc = Start-Process .\googet.exe -ArgumentList @('--noconfirm', "--root=`"$goo_root`"", '--verbose', 'install', $goo.FullName) -NoNewWindow -Wait -PassThru
if ($install_proc.ExitCode -ne 0) {
  Write-Error "Googet install exited with $($install_proc.ExitCode); wanted 0"
  exit $install_proc.ExitCode
}
Write-Host "Successfully installed $($goo.FullName)"

# Remove googet
Write-Host 'Attempting to use googet to remove googet'
$remove_proc = Start-Process "$env:ProgramData\GooGet\googet.exe" -ArgumentList @('--noconfirm', '--verbose', 'remove', 'googet') -NoNewWindow -Wait -PassThru
if ($remove_proc.ExitCode -ne 0) {
  Write-Error "Googet remove exited with $($remove_proc.ExitCode); wanted 0"
  exit $remove_proc.ExitCode
}
Write-Host 'Successfully removed googet'