ARG LOOPFORGE_TEST_BASE=superagent-platform:local
FROM ${LOOPFORGE_TEST_BASE}
RUN apt-get update && apt-get install -y --no-install-recommends sudo \
 && rm -rf /var/lib/apt/lists/* \
 && printf 'node ALL=(root) NOPASSWD: /usr/bin/true, /usr/local/bin/docker\n' > /etc/sudoers.d/installer \
 && chmod 440 /etc/sudoers.d/installer
ENTRYPOINT []
USER node
