# RPC request-write deadline check

This record describes the evidence captured before approach selection.

| Issue | Candidate correction | Observed evidence |
| --- | --- | --- |
| Request submission blocks before the existing response timer starts | Set the existing pipe's write deadline and use that deadline for the response loop | The unmodified client waits 2.01 seconds and reports `Broken pipe` for a 0.25-second request budget |

The regression's fake child acknowledges initialization, then stops reading stdin.
The client submits a one-megabyte request through the same `os.Pipe` transport as normal RPC calls.
The regression requires the existing timeout error and a return before 1.5 seconds.

## Candidate measurement

An isolated copy is in ignored `probe/out/v21-campaign/round3-candidate/`.
It contains the published v2.0.3 source plus the new regression and this proposed change:

```go
method := payload.Value("method").(string)
deadline := time.Now().Add(timeout)
if err := process.stdin.SetWriteDeadline(deadline); err != nil {
    return nil, pyerr.NewOSError(err)
}
```

The response loop uses the same `deadline`.
A write error that wraps `os.ErrDeadlineExceeded` maps to the existing `InstallError` timeout message.
The request encoder and the initialization-only fallback remain unchanged.

The complete installer unit package passes against this candidate in 1.198 seconds.
That run includes the blocked-write regression and all four initialization-recovery and no-replay cases.
The working-tree production file lacked the candidate correction at that checkpoint.

An independent pipe probe also returns `os.ErrDeadlineExceeded` after 26 milliseconds.
It submits 65,536 bytes before expiry. This confirms why an incomplete operation must not be retried.

Logs: ignored `probe/out/v21-campaign/write-deadline-before.log` and `write-deadline-candidate.log`.

The accepted correction now passes the complete installer package in 1.249 seconds.
Decision receipt: `review_5cb221fd75e24ba1a9494b350e9c0195`.
