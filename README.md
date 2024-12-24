### NOTE:

This is a fork from [arlolra/meek](https://github.com/arlolra/meek) and made some changes to work as a standalone service.
#### Note: 
* Don't forget that the client only pass raw data to the third-party service on the server, so it works only if you use this service as tunnel/proxy for third-party client.
* Now a built-in socks5 service added to the meek-server so if you are looking for an easy way to deploy a proxy server this feature can help a lot.
>  Use --help for more information and arguments
#### Changelog:
#### * Server
* Added a socks5 service. (If `-external-service` argument was missing, server automatically user built-in socks5. Also, local port for socks5 is changeable by using `-socks` argument)
* Added `-redirect` argument for 301 response header for non-proxy requests in order to forward user to another service (Helps blocking-resistant)
* Now presented data for non-proxy requests can be loaded form an external file. (if `-mask` provided, the content will be presented, otherwise it will search for index.html file in working directory and if it wasn't available a simple message will appear for user.)

#### Deployment
You can use pre-built executables in release section. If seeking for a safe build or maybe a specific os you can build it yourself by `go build`.