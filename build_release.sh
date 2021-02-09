#!/usr/bin/env bash

DEFAULT_TERRAFORM_VERSION=0.14.6
AVAILABLE_TERRAFORM_VERSIONS="0.12.30 0.13.6 ${DEFAULT_TERRAFORM_VERSION}"

SUFFIX='-betr'
VERSION=${VERSION:-$(sed -n -e 's/^const atlantisVersion = "\(.\+\)"$/\1/p' main.go)}
echo -e "\nCurrent version is $VERSION\n"

if [[ $# == 0 ]]; then
    PATCH=$SUFFIX
else
    PATCH=".$1$SUFFIX"
fi
VERSION="$VERSION$PATCH"

echo "Building atlantis $VERSION"

# Patch version information in main.go
trap 'git restore main.go Dockerfile' EXIT
sed -e "s/^const atlantisVersion = \".\+\"$/const atlantisVersion = \"$VERSION\"/" -i main.go
sed -e "s/^\(ENV DEFAULT_TERRAFORM_VERSION=\).*$/\1$DEFAULT_TERRAFORM_VERSION/" \
    -e "s/\(AVAILABLE_TERRAFORM_VERSIONS=\"\).*\"/\1$AVAILABLE_TERRAFORM_VERSIONS\"/" \
    -i Dockerfile

docker run --rm -v $(pwd):/go/src/github.com/runatlantis/atlantis -w /go/src/github.com/runatlantis/atlantis runatlantis/testing-env make test

env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o atlantis
if [[ $? != 0 ]]; then
    echo -e "\nBuild failed.\n"
    exit 1
fi

echo 'Building docker image'
docker build -t magne/atlantis:$VERSION .
docker tag magne/atlantis:$VERSION magne/atlantis:latest

echo -e "\nTo publish to Docker Hub, execute: docker push magne/atlantis:$VERSION\n"