# GooGet Server

This is a simple example of what a GooGet server looks like. 
The server looks for a folder in it's root directory called 'packages', 
creating it if necesary. The directory contents are read on a set 
interval and all .goo packages served in the repo 'repo'.
You can then point a client at http://localhost:8000/repo, or view 
http://localhost:8000/repo/index in a browser.

Improvements to this design would include only updating the repository on 
a package change as well as providing and api for adding/removing packages.
