build:
	swag init -g services/web.go --instanceName vault \
	&& go build .
