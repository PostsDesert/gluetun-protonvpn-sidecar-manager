module gluetun-proton-manager

go 1.25

require (
	github.com/ProtonMail/go-proton-api v0.0.0-20260109112619-daf7af47921d
	github.com/go-resty/resty/v2 v2.7.0
)

replace github.com/go-resty/resty/v2 => github.com/ProtonMail/resty/v2 v2.0.0-20250929142426-e3dc6308c80b
