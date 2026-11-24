# Sets the owner and permissions of the GooGet root, then adds it to the
# machine GooGetRoot and Path environment variables.
#
# Canonical owner and protected DACL (verified in
# install_script_windows_test.go):
#   O:BAD:PAI(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)
# Only well-known SIDs are used because account names are localized.
param(
  [string]$GooGetRoot = "${env:ProgramData}\GooGet",
  # Leaves the machine GooGetRoot and Path variables unchanged.
  [switch]$SkipEnvironment
)

$ErrorActionPreference = 'Stop'

$administratorsSid = 'S-1-5-32-544'
$systemSid = 'S-1-5-18'
$usersSid = 'S-1-5-32-545'

$inheritFlags = [System.Security.AccessControl.InheritanceFlags]'ContainerInherit, ObjectInherit'
$propagationFlags = [System.Security.AccessControl.PropagationFlags]::None
$allow = [System.Security.AccessControl.AccessControlType]::Allow
$fullControl = [System.Security.AccessControl.FileSystemRights]::FullControl
$readAndExecute = [System.Security.AccessControl.FileSystemRights]::ReadAndExecute
$synchronize = [System.Security.AccessControl.FileSystemRights]::Synchronize
$directory = [System.IO.FileAttributes]::Directory
$reparsePoint = [System.IO.FileAttributes]::ReparsePoint

$rootAccessRules = [ordered]@{
  $administratorsSid = $fullControl
  $systemSid = $fullControl
  $usersSid = $readAndExecute
}

function Resolve-GooGetRootPath([string]$path) {
  if ([string]::IsNullOrWhiteSpace($path)) {
    throw 'GooGetRoot must not be empty.'
  }
  # Path.IsPathRooted accepts drive-relative and root-relative paths, and
  # Path.IsPathFullyQualified does not exist on .NET Framework (PS 5.1).
  # The UNC lookahead rejects Win32 device prefixes (\\?\, \\.\) and dot-only
  # server names (\\..\).
  $isDrivePath = $path -match '^[A-Za-z]:[\\/]'
  $isUncPath = $path -match '^\\\\(?![.?]+\\)[^\\/]+\\[^\\/]+'
  if (-not ($isDrivePath -or $isUncPath)) {
    throw "GooGetRoot must be an absolute path: $path"
  }
  $full = [System.IO.Path]::GetFullPath($path)
  $rootVol = [System.IO.Path]::GetPathRoot($full)
  if ($full.TrimEnd('\', '/') -eq $rootVol.TrimEnd('\', '/')) {
    throw "Refusing to use volume root $full as GooGetRoot."
  }
  return $full.TrimEnd('\', '/')
}

function New-GooGetRootAcl {
  $acl = New-Object System.Security.AccessControl.DirectorySecurity
  $acl.SetAccessRuleProtection($true, $false)
  foreach ($entry in $rootAccessRules.GetEnumerator()) {
    $sid = New-Object System.Security.Principal.SecurityIdentifier($entry.Key)
    $rule = New-Object System.Security.AccessControl.FileSystemAccessRule($sid, $entry.Value, $inheritFlags, $propagationFlags, $allow)
    $acl.AddAccessRule($rule)
  }
  return $acl
}

# Use .NET ACL APIs rather than Get-Acl/Set-Acl, whose module can fail to load
# when PSModulePath is inherited from PowerShell 7. FileSystemAclExtensions is
# used on PowerShell 7 (.NET) and Directory static methods on Windows
# PowerShell 5.1 (.NET Framework).
function New-ProtectedDirectory([string]$path, [System.Security.AccessControl.DirectorySecurity]$acl) {
  if ('System.IO.FileSystemAclExtensions' -as [type]) {
    [System.IO.FileSystemAclExtensions]::CreateDirectory($acl, $path) | Out-Null
  } else {
    [System.IO.Directory]::CreateDirectory($path, $acl) | Out-Null
  }
}

function Get-DirectoryAcl([string]$path) {
  if ('System.IO.FileSystemAclExtensions' -as [type]) {
    return [System.IO.FileSystemAclExtensions]::GetAccessControl((New-Object System.IO.DirectoryInfo($path)))
  }
  return [System.IO.Directory]::GetAccessControl($path)
}

function Set-DirectoryAcl([string]$path, [System.Security.AccessControl.DirectorySecurity]$acl) {
  if ('System.IO.FileSystemAclExtensions' -as [type]) {
    [System.IO.FileSystemAclExtensions]::SetAccessControl((New-Object System.IO.DirectoryInfo($path)), $acl)
  } else {
    [System.IO.Directory]::SetAccessControl($path, $acl)
  }
}

# Uses GetAttributes rather than Test-Path so dangling reparse points are
# reported as present and access errors fail closed instead of looking absent.
function Get-PathAttributesIfPresent([string]$path) {
  try {
    return [System.IO.File]::GetAttributes($path)
  } catch [System.IO.FileNotFoundException], [System.IO.DirectoryNotFoundException] {
    return $null
  }
}

function Assert-RegularDirectory([string]$path) {
  $attrs = [System.IO.File]::GetAttributes($path)
  if ((($attrs -band $directory) -eq 0) -or (($attrs -band $reparsePoint) -ne 0)) {
    throw "Refusing to use $path because it is not a regular directory (attributes: $attrs)."
  }
}

function Assert-RegularFileIfPresent([string]$path) {
  $attrs = Get-PathAttributesIfPresent $path
  if ($null -eq $attrs) {
    return
  }
  if ((($attrs -band $directory) -ne 0) -or (($attrs -band $reparsePoint) -ne 0)) {
    throw "Refusing to use $path because it is not a regular file (attributes: $attrs)."
  }
}

# Returns an empty string if the owner and DACL of $path are canonical, or a
# description of the first difference otherwise.
function Get-GooGetRootAclProblem([string]$path) {
  $acl = Get-DirectoryAcl $path
  $owner = $acl.GetOwner([System.Security.Principal.SecurityIdentifier]).Value
  if ($owner -ne $administratorsSid) {
    return "owner is $owner"
  }
  if (-not $acl.AreAccessRulesProtected) {
    return 'DACL is not protected'
  }
  $expected = @{}
  foreach ($entry in $rootAccessRules.GetEnumerator()) {
    # Windows adds Synchronize to allow entries.
    $expected[$entry.Key] = [int]($entry.Value -bor $synchronize)
  }
  $rules = @($acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]))
  if ($rules.Count -ne $expected.Count) {
    return "DACL has $($rules.Count) entries"
  }
  foreach ($rule in $rules) {
    $sid = $rule.IdentityReference.Value
    if (-not $expected.ContainsKey($sid)) {
      return "unexpected entry for $sid"
    }
    if ($rule.IsInherited -or
        $rule.AccessControlType -ne $allow -or
        $rule.InheritanceFlags -ne $inheritFlags -or
        $rule.PropagationFlags -ne $propagationFlags -or
        [int]$rule.FileSystemRights -ne $expected[$sid]) {
      return "unexpected entry for ${sid}: $($rule.AccessControlType) $($rule.FileSystemRights) $($rule.InheritanceFlags) $($rule.PropagationFlags) inherited=$($rule.IsInherited)"
    }
    $expected.Remove($sid)
  }
  return ''
}

function Set-GooGetRootPermissions([string]$root) {
  $root = Resolve-GooGetRootPath $root
  if ($null -eq (Get-PathAttributesIfPresent $root)) {
    New-ProtectedDirectory $root (New-GooGetRootAcl)
    Write-Host "Created $root."
  }
  $googetExe = Join-Path $root 'googet.exe'
  Assert-RegularDirectory $root
  Assert-RegularFileIfPresent $googetExe

  Write-Host "Setting permissions on $root."
  # icacls /setowner enables SeTakeOwnershipPrivilege/SeRestorePrivilege to
  # reassign ownership from another principal (.NET SetOwner does not); /L
  # avoids following a reparse point.
  $icacls = Join-Path $env:SystemRoot 'System32\icacls.exe'
  $global:LASTEXITCODE = 0
  & $icacls $root /setowner "*$administratorsSid" /L /Q
  if ($LASTEXITCODE -ne 0) {
    throw "icacls /setowner failed on $root with exit code $LASTEXITCODE."
  }

  Assert-RegularDirectory $root
  # Replaces the DACL atomically when the owner has WRITE_DAC; .NET
  # SetAccessControl does not enable SeRestorePrivilege.
  Set-DirectoryAcl $root (New-GooGetRootAcl)

  $problem = Get-GooGetRootAclProblem $root
  if ($problem) {
    $sddl = (Get-DirectoryAcl $root).GetSecurityDescriptorSddlForm('Owner, Access')
    throw "Permissions on $root were not applied ($problem): $sddl"
  }
  # The read-back follows a reparse point, so recheck the root and googet.exe
  # after the protected DACL is verified and before updating the environment.
  Assert-RegularDirectory $root
  Assert-RegularFileIfPresent $googetExe
}

function Set-GooGetEnvironment([string]$root) {
  $machineEnv = 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment'
  if ((Get-ItemProperty -LiteralPath $machineEnv).GooGetRoot -ne $root) {
    Set-ItemProperty -LiteralPath $machineEnv -Name 'GooGetRoot' -Value $root
  }
  $path = (Get-ItemProperty -LiteralPath $machineEnv).Path
  if ($path -notlike "*${root}*") {
    Set-ItemProperty -LiteralPath $machineEnv -Name 'Path' -Value ($path + ";${root}")
  }
}

if ($MyInvocation.InvocationName -ne '.') {
  try {
    $root = Resolve-GooGetRootPath $GooGetRoot
    Set-GooGetRootPermissions $root
    if (-not $SkipEnvironment) {
      Set-GooGetEnvironment $root
    }
  } catch {
    [Console]::Error.WriteLine(($_ | Out-String -Width 4096))
    exit 1
  }
}
