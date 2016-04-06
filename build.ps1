if (!(gc googet.goospec | where {$_ -match '"version": "(.+)",'})) {
  throw 'Error grabbing version from goospec'
}
$version = $matches[1]

go build -ldflags "-X main.version=$version"
