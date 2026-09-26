#!/bin/sh
# Sonarr: Settings, Connect, Custom Script. Leave Arguments empty.
# Tick On Import, On Upgrade and On Import Complete. Test runs a tool check.
exec amuxify --profile "${AMUXIFY_PROFILE:-homelab}" hook sonarr
