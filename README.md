# GooGet

[![Go](https://github.com/google/googet/actions/workflows/go.yml/badge.svg)](https://github.com/google/googet/actions/workflows/go.yml)

GooGet (Googet's Obviously Only a Goofy Experimental Title) is a modular
package repository solution primarily designed for Windows.

GooGet is shipped with the official Google Cloud Platform Windows images and is
used to maintain the guest environment.

## Build

To build and package googet run

```
go run goopack/goopack.go googet.goospec
```

This will result in googet.exe and googet.x86_64.VERSION.goo which can be installed on a
machine with the `googet install` command (assuming googet is already
installed).

To install on a fresh machine copy both googet.exe and the googet package
over and run:

```
googet -root 'c:/ProgramData/GooGet' install googet googet.x86_64.VERSION.goo
```

## Conf file

GooGet has the ability to use a conf file to change a few of the default settings.
Place a file named googet.conf in the googet root, which by default is
`C:\ProgramData\GooGet` and configurable by the `-root` flag.


```
proxyserver: http://address_to_proxy:port
archs: [noarch, x86_64]
cachelife: 10m
stalltimeout: 5m
installtimeout: 4h
```

`stalltimeout` (a Go duration such as `5m` or `30s`, default `2m`; zero or
negative values keep the default) is how long googet waits for a server to
respond, or for more data during a download, before it treats the transfer as
stalled. Raise it if your proxy scans large files before responding.

`installtimeout` limits how long googet lets an installer, uninstaller or verify
command run before it and the processes it started are killed (default `4h`;
`0` or a negative value disables it, and an invalid value keeps the default);
goopack build steps always use the default.

On Windows the command runs in a job object, so killing googet also kills the
command and the processes it started. MSI and wusa work done by the Windows
Installer service runs outside the job and is not killed. The first Ctrl-C or
Ctrl-Break while the command runs kills the job at once, and each later one
tries again: GUI installers get no console events, and console ones get no
grace period. googet then runs no
further commands, finishes its cleanup and exits with code `0xC000013A`, as
Ctrl-C would have. Closing the console, logging off or shutting down is not
handled, since Windows ends googet a few seconds later, too soon for a reliable
cleanup. Ctrl-C while no command runs still ends googet at once. If the job
cannot be created, for example because a parent job forbids it, only the
command itself is killed and the processes it started keep running.

On Unix the command runs in its own process group. SIGINT, SIGTERM, SIGHUP and
SIGQUIT sent to googet (for example by Ctrl-C) are forwarded to the group, and a
second one kills it. googet then runs no further commands, finishes its
cleanup and exits from the signal (for SIGQUIT the Go runtime instead dumps its
goroutines and exits with status 2). A SIGKILL to googet leaves the group
running. Since the group is not in the terminal's foreground, Ctrl-Z stops only
googet, and a command that reads from the terminal is stopped until the timeout
kills it. Descendants that call setsid or setpgid leave the group and escape.

On either system, a signal that arrives after the command itself has exited,
while googet still waits for output held open by a process it started, is not
passed on: those processes keep running and the command's result stands, but
googet runs no further commands.

## Repo file

GooGet has the ability to use a repo file to change some repo specific settings.
The file is named your-repo-name.repo by default. It can be used to set the
source URL and indicate that the client should pass authorization headers in 
requests to the source URL.

```
URL: https://foo.com/googet/bar
  useoauth: true
```

## Google Cloud Storage as a back-end

Googet supports using Google Cloud Storage as its server.

```
set GOOREPO=%TEMP%\googet-repo
set REPONAME=my_repo
mkdir %GOOREPO%\%REPONAME%
mkdir %GOOREPO%\packages
go run goopack/goopack.go googet.goospec
copy *.goo %GOOREPO%\packages
go run server\gooserve.go -root %GOOREPO% -save_index %GOOREPO%\%REPONAME%\index
gcloud storage buckets create --project my-project gs://my-googet-server
gcloud storage rsync --recursive %GOOREPO% gs://my-googet-server
googet addrepo gcs gs://my-googet-server

rem This command should print 'gcs: gs://my-googet-server'
googet listrepos

rem This command should list the googet package and any other packages in your repo
googet available -sources gs://my-googet-server/

```

Note that you must regenerate the index and re-upload it to your bucket each time
you add or change packges.
