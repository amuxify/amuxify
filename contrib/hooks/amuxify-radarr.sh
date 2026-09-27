#!/bin/sh
# Radarr: Settings, Connect, Custom Script. Leave Arguments empty.
# Tick On Import and On Upgrade. Test runs a tool check.
exec amuxify --profile "${AMUXIFY_PROFILE:-homelab}" hook radarr
