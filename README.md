# Overview

Tools for looking at Github repos

# Installation

```sh
> go get -u github.com/jabley/project-status
```

# Usage

You will need a [Github token with read permission](https://github.com/settings/tokens)
for (private) repostories for the user / organisation that you're interested in.

(Running the binary version):
```sh
> GH_ORG=MyOrgOrUser GH_OAUTH_TOKEN=MyToken project-status
```

(Running the development version):
```sh
> GH_ORG=MyOrgOrUser GH_OAUTH_TOKEN=MyToken go run main.go
```

