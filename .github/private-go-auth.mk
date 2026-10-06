# Pass only variable names so build logs never expand dependency credentials.
# GIT_CONFIG_PARAMETERS also works with the legacy SDK builder's Git.
PRIVATE_GO_DOCKER_ENV := -e GOPRIVATE -e GO_DEPENDENCY_TOKEN -e GIT_CONFIG_PARAMETERS
PRIVATE_GO_SUDO_ENV := --preserve-env=GOPRIVATE,GO_DEPENDENCY_TOKEN,GIT_CONFIG_PARAMETERS
