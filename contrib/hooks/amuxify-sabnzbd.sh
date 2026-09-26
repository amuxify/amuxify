#!/bin/sh
# SABnzbd: put this file in the scripts folder and assign it per category.
# Enable "script_can_fail" in Config, Special to let exit 1 fail the job.
exec amuxify --profile "${AMUXIFY_PROFILE:-homelab}" hook sabnzbd "$@"
