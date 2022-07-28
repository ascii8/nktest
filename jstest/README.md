# notes

the nakama client has issues with using the `xmlhttprequest` package, as it
sets a non-standard credentials option (`cocos-ignore`). this is why the script
here does not do a package import (`require('nakama-js')`), but instead imports
the `nakama-js.cjs.js` file (`require('./nakama-js.cjs.js')`).

when updating the nakama client lib the portion with the `cocos-ignore` will
need to be manually commented out.

## using

```sh
# run tests from in project root
$ KEEP=1h go test -v -timeout=2h

# run socat (to forward and log traffic)
$ socat -v -d -d tcp-listen:8080,bind=127.0.0.1,fork tcp:127.0.0.1:<port>

# run main.ts
$ ./main.ts --port 8080 --key nktest_server
```
