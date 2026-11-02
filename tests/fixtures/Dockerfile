FROM node:22-bookworm-slim
RUN apt-get -o Acquire::http::Timeout=20 -o Acquire::Retries=0 update \
    && apt-get -o Acquire::http::Timeout=20 -o Acquire::Retries=0 install -y --no-install-recommends iptables passwd util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system PlexLocalBridge \
    && useradd --system --gid PlexLocalBridge --no-create-home PlexLocalBridge \
    && groupadd --system administrators \
    && usermod -a -G administrators node \
    && mkdir -p /usr/syno/synoman/webman/modules
COPY mock-plex.mjs /mock-plex.mjs
COPY authenticate.cgi /usr/syno/synoman/webman/modules/authenticate.cgi
RUN chmod 755 /usr/syno/synoman/webman/modules/authenticate.cgi
CMD ["node", "/mock-plex.mjs"]
