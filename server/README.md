# GooGet Server

This is a simple example of what a GooGet server looks like.
The server looks for a folder in it's root directory called 'packages',
creating it if necesary. The directory contents are read on a set
interval and all .goo packages served in the repo 'repo'.
You can then point a client at http://localhost:8000/repo, or view
http://localhost:8000/repo/index in a browser.

Improvements to this design would include only updating the repository on
a package change as well as providing and api for adding/removing packages.

The server code can also be used to generate a package index from files in
Google Cloud Storage, note that this functionality only works with writing
out the index, not serving the index via the inbuilt webserver.

```cmd
go run gooserve.go -package_path gs://my-bucket/goorepo -save_index gs://my-bucket/goorepo/myrepo/index
```

WARNING: If you use Powershell and -dump_index instead of -save_index, make
sure to save the file as UTF-8. If you see an error like *ERROR: 2018/05/26
09:23:56.329402 client.go:100: error reading repo "gs://my-bucket/googet/":
invalid character 'ÿ' looking for beginning of value*, that's likely the
problem.

```powershell
# Don't do this, your index file will be UTF-16, which googet won't handle
go run gooserve.go -root /tmp/goorepo/ -dump_index > /tmp/goorepo/index
# Preserving the encoding fixes the problem
go run gooserve.go -root /tmp/goorepo/ -dump_index | Out-File index -Encoding OEM
```
