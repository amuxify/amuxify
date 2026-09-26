#!/bin/sh

##############################################################################
### NZBGET POST-PROCESSING SCRIPT                                          ###

# Verify and sanitise the finished download with amuxify.
#
# amuxify scans every file, rebuilds video into verified MKV or cleans it in
# place, and fails the job when a file fails or is blocked.

##############################################################################
### OPTIONS                                                                ###

# Profile (homelab, anime, archive, strict, or a path to a TOML file).
#Profile=homelab

# Verdict that fails the job (warn, fail, block).
#FailOn=fail

### NZBGET POST-PROCESSING SCRIPT                                          ###
##############################################################################

exec amuxify --profile "${NZBPO_PROFILE:-${AMUXIFY_PROFILE:-homelab}}" hook nzbget --fail-on "${NZBPO_FAILON:-fail}"
