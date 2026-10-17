# Which result type do I use?

go-common has several ways to say how a call went. They answer different
questions, and they are meant to be used together. They are not one type with
several spellings, so there is deliberately no single "outcome" union to convert
between.

| The question | Use | What it carries |
| --- | --- | --- |
| What error do I return to my own caller, with a status and a stable code? | [`errors`](../errors/) (`errors.New(status, code, msg)`) | HTTP status, a dotted code clients match on, a message, a cause |
| An enricher looked something up: what did it find? | [`meshresult`](../meshresult/) | `ok`, `no_data`, `unreachable`, `timeout`, `error`, mapped to the HTTP code the consumer keys on |
| The request worked, but part of the answer is missing because a sibling is unavailable | [`degraded`](../degraded/) | Tokens such as `ledger-down` in the response envelope's `degraded` list |
| A stored field has no value | [`nullify`](../nullify/) | `NULL` instead of the string `"unknown"` |
| Should this failure be retried? | `resilience.Retryable(err)` | A yes or no; any error type can implement `Retryable() bool` |
| I am calling a sibling service | [`meshclient`](../meshclient/), optionally with a [`resilience.Policy`](../resilience/) | Records `degraded` tokens itself in fail-open mode, and says whether a failure is retryable |

## They layer

One request can use all of them. An enricher that asks a sibling for a country:

1. It calls the sibling through `meshclient` in `FailOpen` mode. If the sibling is
   down, the client records `geo-down` in the request's `degraded` list and returns
   an error that satisfies `meshclient.IsUnavailable`.
2. The enricher carries on without the country. Its answer is still `meshresult.OK`
   (it found everything it could), and the response lists `geo-down` under
   `degraded`, so the consumer knows the data is partial.
3. If instead the enricher's whole target is unreachable, that is
   `meshresult.Unreachable`, an answer about the target and not a failure of the
   enricher, and `degraded` stays empty.
4. If the caller sent a bad request, that is an `errors.Error` with status 400,
   whatever happened to the siblings.

The axes are orthogonal on purpose: "partial because a sibling was down" (degraded)
is a different fact from "the target has nothing" (meshresult.NoData), and both are
different from "your request was wrong" (errors).

## Whether to retry

`resilience.Retryable` decides, in this order: the error's own `Retryable() bool`
method if it has one, the `retry.Transient` marker, a fleet `errors.Error` by its
HTTP status (429, 408, 425 and 5xx except 501 and 505), then network failures such
as timeouts and refused connections. Cancellation is never retried, and an error it
does not recognise is not retried either, because retrying something unexplained is
how a bug becomes an outage.

A retry is only taken if the policy's retry budget has a token, so retries stay a
small fraction of traffic when an upstream is failing. See the package comment of
[`resilience`](../resilience/).
