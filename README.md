# etcd-config-dispenser

Some things are best explained with an example: I use [letsencrypt-with-etcd](https://github.com/Jille/letsencrypt-with-etcd) to write new certificates to etcd, and then use etcd-config-dispenser to get the certificates as files in my nginx containers. It sends SIGHUP to nginx whenever they've changed. This command reads all values in /certificates from etcd and writes them to /config on disk. It starts nginx right after creating the files, and sends nginx SIGHUP whenever they've changed.

```
etcd-config-dispenser --prefix=/certificates --target=/config --signal=SIGHUP -- /usr/sbin/nginx -g "daemon off;"
```

You can use this as secret or config distribution when you don't have Kubernetes to do it for you.

Docker Swarm supports secrets, but they're immutable and there's no way to change them without stopping your container.

I use this to deliver config and certificates to nginx and postgresql without having to restart them for changes.

## Building it into your Dockerfile

If you're using Docker, this is pretty easy to use in your Dockerfile using a multi-stage build:

```
FROM ghcr.io/jille/etcd-config-dispenser:master AS etcd-config-dispenser
FROM your:base
COPY --from=etcd-config-dispenser /bin/etcd-config-dispenser /usr/local/sbin/etcd-config-dispenser

...

CMD ["/usr/local/sbin/etcd-config-dispenser", "--prefix=/certificates", "--target=/config", \
     "--signal=SIGHUP", "--", "/usr/sbin/nginx", "-g", "daemon off;"]
```

## Parameters

Parameters about what to write where is done through flags:

- `--prefix` (`-p`) Prefix in etcd of all files to read.
- `--target` (`-t`) Filesystem path to write files to.
- `--signal` (`-s`) Name of the signal to the subprocess on change (for example SIGHUP).
- `--mode` (`-m`) Mode (as in chmod) of the written files (default 0600).
- `--owner` (`-o`) Owner of the written files (can only be set by root, default: current user)
- `--group` (`-g`) Group of the written files (can only be set by root, default: current group)

Configuration for connecting to etcd is passed in through environment variables. It takes these settings:

- ETCD_ENDPOINTS is where to find your etcd cluster
- ETCD_USERNAME and ETCD_PASSWORD are used to connect to etcd. No authentication is used if you leave them unset/empty.

See https://github.com/Jille/etcd-client-from-env for more parameters for connecting to etcd.

## Atomicity

etcd-config-dispenser writes out all new changes that happened in an etcd transaction, and then signals your binary. This means that if you write a new private key and certificate in the same transaction, nginx will only be reloaded after both have been written.

There is a small window between writing the two files, and if something else were to reload nginx at that exact moment, it would read inconsistent results. Feel free to file a feature/pull request if that is a problem for you (and we can switch to Kubernetes' trick of writing the new files elsewhere and flipping a single symlink).

## Failing gracefully

You don't want a global crash when your etcd restarts, right? etcd-config-dispenser has been written with graceful failure handling. It will log the error, and keep trying to recover to resume normal operations. I recommend you to add monitoring to see if your files aren't getting updated anymore when you accidentally removed the etcd user.
