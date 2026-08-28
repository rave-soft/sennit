Get stdout/stderr from a background shell by ID; set wait=true to block until completion.

wait=true does not block unconditionally: if the person sends a new message while waiting, the call returns early with "Wait interrupted" instead of the usual "Status: running"/"Status: completed". When that happens, stop and respond to the person — do not call job_output again to resume waiting. The background job keeps running and its ID stays valid for a later job_output call.
