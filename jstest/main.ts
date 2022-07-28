#!/usr/bin/env -S yarn run ts-node

global.XMLHttpRequest = require('xmlhttprequest').XMLHttpRequest;
global.WebSocket = require('ws').WebSocket;
process.removeAllListeners('warning');

var nakama = require('./nakama-js.cjs.js');
var uuid = require('uuid');
var minimist = require('minimist');

(async function() {
  // parse args
  var args = minimist(process.argv.slice(2), {
      string: ['key', 'host', 'deviceId'],
      int: ['port'],
      boolean: ['ssl', 'debug'],
      default: {
        key: 'defaultkey',
        host: '127.0.0.1',
        port: 7350,
        ssl: false,
        debug: false,
        deviceId: uuid.v4(),
      },
  });

  console.log(args);

  // create client
  var cl = new nakama.Client(args.key, args.host, args.port, args.ssl);
  // start session
  var session = await cl.authenticateDevice(args.deviceId, true);
  console.log('userId', session.user_id);
  // open socket
  var socket = cl.createSocket(args.ssl, args.debug);
  await socket.connect(session);
})();
