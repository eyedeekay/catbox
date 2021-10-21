![terrarium](doc/terrarium-with-text.png)

[![Build
Status](https://travis-ci.org/eyedeekay/terrarium.svg)](https://travis-ci.org/eyedeekay/terrarium)
[![Go Report
Card](https://goreportcard.com/badge/i2pgit.org/idk/terrarium)](https://goreportcard.com/report/i2pgit.org/idk/terrarium)

terrarium is an IRC server with a focus on being small and understandable. The
goal is security.


# Features
* Server to server linking
* IRC operators
* Private (WHOIS shows no channels, LIST isn't supported)
* Flood protection
* K: line style connection banning
* TLS

terrarium implements enough of [RFC 1459](https://tools.ietf.org/html/rfc1459)
to be recognisable as IRC and be minimally functional. I likely won't add
much more and don't intend it to be complete. If I don't think something is
required it likely won't be here.


# Installation
1. Download terrarium from the Releases tab on GitHub, or build from source
   (`go build`).
2. Configure terrarium through config files. There are example configs in the
   `conf` directory. All settings are optional and have defaults.
3. Run it, e.g. `./terrarium -conf terrarium.conf`. You might run it via systemd
   via a service such as:

```
[Service]
ExecStart=/home/ircd/terrarium/terrarium -conf /home/ircd/terrarium/terrarium.conf
Restart=always

[Install]
WantedBy=default.target
```


# Configuration

## terrarium.conf
Global server settings.


## opers.conf
IRC operators.


## servers.conf
The servers to link with.


## users.conf
Privileges and hostname spoofs for users.

The only privilege right now is flood exemption.


## TLS
A setup for a network might look like this:

* Give each server a certificate with 2 SANs: Its own hostname, e.g.
  server1.example.com, and the network hostname, e.g. irc.example.com.
* Set up irc.example.com with DNS round-robin listing each server's IP.
* List each server by its own hostname in servers.conf.

Clients connect to the network hostname and verify against it. Servers
connect to each other by server hostname and verify against it.


# Why the name?
My domain name is summercat.com, cats love boxes, and a tribute to
ircd-ratbox, the IRC daemon I used in the past.


# Logo
terrarium logo (c) 2017 Bee
