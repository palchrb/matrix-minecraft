#!/bin/bash
if [[ -z "$GID" ]]; then
	GID="$UID"
fi

function fixperms {
	chown -R $UID:$GID /data
	# /usr/bin/matrix-minecraft is read-only, so disable file logging if it points there.
	if [[ "$(yq e '.logging.writers[1].filename' /data/config.yaml)" == "./logs/matrix-minecraft.log" ]]; then
		yq -I4 e -i 'del(.logging.writers[1])' /data/config.yaml
	fi
}

if [[ ! -f /data/config.yaml ]]; then
	/usr/bin/matrix-minecraft -c /data/config.yaml -e || exit $?
	echo "Didn't find a config file."
	echo "Copied default config file to /data/config.yaml"
	echo "Modify that config file to your liking."
	echo "Start the container again after that to generate the registration file."
	exit
fi

if [[ ! -f /data/registration.yaml ]]; then
	/usr/bin/matrix-minecraft -g -c /data/config.yaml -r /data/registration.yaml || exit $?
	echo "Didn't find a registration file."
	echo "Generated one for you."
	echo "See https://docs.mau.fi/bridges/general/registering-appservices.html on how to use it."
	exit
fi

cd /data
fixperms

# Create user/group so supplementary groups (e.g. docker) work
addgroup -g $GID mcbridge 2>/dev/null
adduser -D -u $UID -G mcbridge -h /data mcbridge 2>/dev/null

# If docker socket exists, ensure our user can access it
if [ -S /var/run/docker.sock ]; then
	DOCKER_SOCK_GID=$(stat -c '%g' /var/run/docker.sock)
	addgroup -g $DOCKER_SOCK_GID docker 2>/dev/null
	addgroup mcbridge docker 2>/dev/null
fi

exec su-exec $UID:$GID /usr/bin/matrix-minecraft
