# GooGet

[![Build Status](https://travis-ci.org/google/googet.svg?branch=master)](https://travis-ci.org/google/googet)

GooGet (Googet's Obviously Only a Goofy Experimental Title) is a modular
package repository solution primarily designed for Windows.

GooGet is shipped with the official Google Cloud Platform Windows images and is
used to maintain the guest environment.

This is not an official Google product.

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
gsutil mb --project my-project my-googet-server
gsutil rsync -r %GOOREPO% gs://my-googet-server
./googet.exe addrepo gcs gs://my-googet-server

rem This command should print 'gcs: gs://my-googet-server'
./googet.exec listrepos

rem This command should list the googet package and any other packages in your repo
./googet.exe available -sources gs://my-googet-server/

```

Note that you must regenerate the index and re-upload it to your bucket each time
you add or change packges.
