## MEEK 
**A blocking-resistant proxy based on http/1.1, h2.**

This is a fork from [arlolra/meek](https://github.com/arlolra/meek) and made some changes to work as a standalone service.
#### Note: 
* Don't forget that the service only pass client's raw data to the third-party service on the server, so it works only if you use this as a tunnel/proxy service for third-party service on client-side.


## Changelog:
### Server
* Works as a standalone service
* Added a socks5 service. (If `-external-service` argument was missing, server automatically uses built-in socks5. Also, local port for socks5 is changeable by using `-socks` argument)
* Added `-redirect` argument for 301 response header for non-proxy requests in order to forward user to another location (Helps blocking-resistant). Keep in mind that this option will override `-mask`.
* Now presented data for non-proxy requests can be loaded form an external file. (if `-mask` provided, the content of provided file will be presented, otherwise it will search for index.html file in working directory and if it wasn't available a simple message will appear for user.)
### Client
* Works as a standalone service
* You should use an external service like [Project X](https://github.com/XTLS/Xray-core) to communicate with server if you are using built-in socks5 option. the config file `config.json` for `Project X` is also available and can be used like `./xray -c config.json` (this config file serve a service with socks5 proxy on port `1080` and http proxy on `8080`).
### Deployment
You can use pre-built executables in release section. If seeking for a safe build or maybe a specific os you can build it yourself by `go build`.
### Run
> Note: use `--help` for more advanced options.

Just put executable anywhere and run it by one of the following commands.
#### Server
* Enabled tls: `./meek-server -cert cert.crt -key private.key -port 443`
* Disabled tls: `./meek-server -disable-tls -port 80`
#### Client
* Domain based: `./meek-client -url https://example.com:443 -port 4456`
* IP based: `./meek-client -url http://1.2.3.4:80 -port 4456`

