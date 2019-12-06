$googet_root = "${env:ProgramData}\GooGet"
$machine_env = 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment'

if ((Get-ItemProperty $machine_env).GooGetRoot -ne $googet_root) {
  Set-ItemProperty $machine_env -Name 'GooGetRoot' -Value $googet_root
}

$path = (Get-ItemProperty $machine_env).Path
if ($path -notlike "*${googet_root}*") {
  Set-ItemProperty $machine_env -Name 'Path' -Value ($path + ";${googet_root}")
}

# Set permisons on GooGet root
Start-Process icacls.exe -ArgumentList @($googet_root, '/grant:r', 'Administrators:(OI)(CI)F', '/grant:r', 'SYSTEM:(OI)(CI)F', '/grant:r', 'Users:(OI)(CI)RX', '/inheritance:r')
