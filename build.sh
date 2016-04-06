#! /bin/sh
version=$(cat googet.goospec | grep -Po '"version":\s+"\K.+(?=",)')
if [[ $? -ne 0 ]]; then
  echo "could not match verson in goospec"
  exit 1
fi
GOOS=windows go build -ldflags "-X main.version=2.7.1@1"
