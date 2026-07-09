module github.com/tencentcloud/CubeSandbox/CubeOps

go 1.25.7

require (
	github.com/golang-jwt/jwt/v5 v5.2.2
	github.com/google/uuid v1.6.0
	github.com/gorilla/mux v1.8.0
	github.com/tencentcloud/CubeSandbox/cubedb v0.1.0
	golang.org/x/crypto v0.50.0
	gorm.io/gorm v1.25.2
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/go-sql-driver/mysql v1.9.3 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/pressly/goose/v3 v3.27.1 // indirect
	github.com/sethvargo/go-retry v0.3.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	gorm.io/driver/mysql v1.5.1 // indirect
)

replace github.com/tencentcloud/CubeSandbox/cubedb => ../cubedb
