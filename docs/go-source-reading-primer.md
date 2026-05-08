# Go Source Reading Primer

This doc explains the Go features you will see most often while reading Buckit/MinIO source code. It is written for reading and understanding the codebase, not for becoming fluent in every corner of Go.

## 1. Packages and Imports

Every Go file starts with a package name:

```go
package cmd
```

Files in the same package can use each other's exported and unexported names. Most Buckit server code lives in `cmd`, so a function in `cmd/object-handlers.go` can call helpers in `cmd/erasure-object.go` without importing them.

Imports bring in other packages:

```go
import (
    "context"
    "net/http"

    "github.com/buckit-io/buckit/internal/logger"
)
```

Names from another package are accessed with the package prefix:

```go
logger.LogIf(ctx, err)
http.ResponseWriter
```

## 2. Exported vs Unexported Names

Go uses capitalization for visibility.

| Name | Meaning |
|---|---|
| `ObjectLayer` | Exported from the package; other packages can use it. |
| `erasureObjects` | Unexported; only code in the same package can use it. |
| `GetObjectInfo` | Exported method/function. |
| `getObjectInfo` | Unexported helper. |

In this repo, many important implementation types are lowercase because they are internal to `cmd`, for example `erasureServerPools`, `erasureSets`, and `erasureObjects`.

## 3. Functions

A normal function looks like:

```go
func getObjectInfo(bucket, object string) (ObjectInfo, error) {
    // ...
}
```

The return values are written after the parameter list. This function returns two values: an `ObjectInfo` and an `error`.

Go commonly returns `(value, error)`:

```go
objInfo, err := objectAPI.GetObjectInfo(ctx, bucket, object, opts)
if err != nil {
    return err
}
```

There are no exceptions in normal Go code. Errors are values, and callers check them explicitly.

## 4. Methods

Methods are functions attached to a type. The receiver appears before the method name:

```go
func (z *erasureServerPools) GetObjectInfo(ctx context.Context, bucket, object string, opts ObjectOptions) (ObjectInfo, error) {
    // ...
}
```

Read this as:

```text
GetObjectInfo is a method on *erasureServerPools
```

The receiver name is usually short:

| Receiver | Usually means |
|---|---|
| `z` | Storage layer / erasure pool receiver. |
| `s` | Server, set, or system receiver. |
| `er` | Erasure object receiver. |
| `client` | RPC/client receiver. |

`*erasureServerPools` means a pointer to `erasureServerPools`, so the method can read and mutate the original object instead of a copy.

## 5. Structs

A struct groups fields:

```go
type ObjectInfo struct {
    Bucket string
    Name   string
    Size   int64
}
```

Create one with field names:

```go
info := ObjectInfo{
    Bucket: bucket,
    Name:   object,
    Size:   size,
}
```

Access fields with dot syntax:

```go
info.Size
```

Structs are used heavily for request options, metadata, disk info, and object info.

## 6. Pointers

`*T` means "pointer to T".

```go
var objAPI ObjectLayer
var pools *erasureServerPools
```

`&value` means "address of value":

```go
return &erasureServerPools{serverPools: pools}
```

Pointers matter because:

- large structs are not copied every time;
- methods can mutate shared state;
- `nil` can mean "not initialized" or "not found".

Common pattern:

```go
if objectAPI == nil {
    return errServerNotInitialized
}
```

## 7. Interfaces

An interface says "any type with these methods can be used here."

Example shape:

```go
type StorageAPI interface {
    ReadFile(ctx context.Context, volume, path string, offset int64, buf []byte, verifier *BitrotVerifier) (int64, error)
    WriteAll(ctx context.Context, volume, path string, b []byte) error
}
```

If `xlStorage` has those methods, it implements `StorageAPI`. If `storageRESTClient` also has those methods, it also implements `StorageAPI`.

That is how the same code can call:

```go
disk.ReadFile(...)
```

without caring whether `disk` is local disk I/O (`xlStorage`) or a remote RPC client (`storageRESTClient`).

Important idea:

```text
Interfaces describe behavior, not inheritance.
```

There is no `implements` keyword. A type implements an interface automatically when it has the required methods.

## 8. Type Assertions and Type Switches

Sometimes code has an interface value and needs the concrete type.

```go
if z, ok := objAPI.(*erasureServerPools); ok {
    // objAPI is really *erasureServerPools here
}
```

Read this as:

```text
Try to treat objAPI as *erasureServerPools.
If it works, ok is true.
```

A type switch handles multiple possible concrete types:

```go
switch v := value.(type) {
case *xlStorage:
    // local disk
case *storageRESTClient:
    // remote disk
default:
    // unknown implementation
}
```

## 9. Short Variable Declaration

This is one of the most common Go forms:

```go
objInfo, err := getObjectInfo(...)
```

`:=` declares new variables and assigns values.

This is different from `=`:

```go
err = doSomething()
```

Use `:=` when creating at least one new variable. Use `=` when assigning to existing variables.

Common pattern:

```go
if err := checkRequestAuthType(ctx, r, policy.GetObjectAction, bucket, object); err != nil {
    writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
    return
}
```

Here `err` only exists inside the `if` block.

## 10. Multiple Return Values

Go functions often return multiple values:

```go
data, err := readConfig()
```

Some functions return a value plus a boolean:

```go
value, ok := cache[key]
if !ok {
    // key was not present
}
```

Some functions return several pieces of data:

```go
poolIdx, setIdx, err := findDiskIndex(...)
```

When a return value is not needed, Go uses `_`:

```go
_, err := io.Copy(dst, src)
```

## 11. Errors

Go error handling is explicit:

```go
if err != nil {
    return err
}
```

Errors can be wrapped with context:

```go
return fmt.Errorf("load bucket metadata: %w", err)
```

`%w` wraps the original error so callers can still inspect it with `errors.Is` or `errors.As`.

Common Buckit/MinIO style:

```go
if err != nil {
    logger.LogIf(ctx, err)
    return err
}
```

## 12. `context.Context`

You will see `ctx context.Context` almost everywhere:

```go
func (z *erasureServerPools) GetObjectInfo(ctx context.Context, bucket, object string, opts ObjectOptions) (ObjectInfo, error)
```

`context.Context` carries:

- cancellation;
- deadlines/timeouts;
- request-scoped values;
- logging/request metadata.

If the client disconnects, the request context may be canceled. Long-running operations should notice and stop.

Common usage:

```go
select {
case <-ctx.Done():
    return ctx.Err()
default:
}
```

Read `<-ctx.Done()` as "wait until the context is canceled."

## 13. `defer`

`defer` runs a function when the current function returns:

```go
f, err := os.Open(path)
if err != nil {
    return err
}
defer f.Close()
```

This guarantees cleanup even if the function returns early.

Common uses:

- close files;
- unlock mutexes;
- release object locks;
- stop timers;
- record metrics after a function finishes.

Example:

```go
lock.Lock()
defer lock.Unlock()
```

## 14. Slices

A slice is a dynamic view over an array:

```go
disks := []StorageAPI{}
```

Append:

```go
disks = append(disks, disk)
```

Length:

```go
len(disks)
```

Index:

```go
disk := disks[i]
```

Range loop:

```go
for i, disk := range disks {
    // i is index, disk is value
}
```

Important: a slice can contain `nil` entries:

```go
for _, disk := range disks {
    if disk == nil {
        continue
    }
}
```

In this codebase, slices often represent disks in an erasure set, pools, endpoints, or peer clients.

## 15. Maps

A map is a key/value table:

```go
metadata := map[string]string{}
metadata["content-type"] = "text/plain"
```

Read with existence check:

```go
value, ok := metadata["content-type"]
if ok {
    // key exists
}
```

Maps are used for:

- HTTP headers;
- user metadata;
- system metadata;
- config key/value data;
- caches.

## 16. Range Loops

`range` iterates over slices, maps, strings, and channels.

Slice:

```go
for i, disk := range disks {
    _ = i
    _ = disk
}
```

Map:

```go
for key, value := range metadata {
    _ = key
    _ = value
}
```

Only values:

```go
for _, disk := range disks {
    _ = disk
}
```

Only indexes:

```go
for i := range disks {
    _ = disks[i]
}
```

## 17. Goroutines

A goroutine is a lightweight concurrent function:

```go
go func() {
    doWork()
}()
```

Buckit/MinIO uses goroutines for:

- reading many disks in parallel;
- sending RPCs to peer nodes;
- background heal/scanner work;
- async notification/replication work.

Important: goroutines run concurrently, so shared data must be protected by locks, channels, or careful ownership.

## 18. Channels

Channels pass values between goroutines.

```go
ch := make(chan ObjectInfo)
```

Send:

```go
ch <- objInfo
```

Receive:

```go
objInfo := <-ch
```

Close:

```go
close(ch)
```

Range over a channel:

```go
for objInfo := range ch {
    // receives until channel is closed
}
```

Channels show up in streaming, metrics, background workers, and notification paths.

## 19. `select`

`select` waits on multiple channel operations:

```go
select {
case item := <-ch:
    return item
case <-ctx.Done():
    return ctx.Err()
}
```

Read this as:

```text
Wait for either work to arrive or the request to be canceled.
```

This is common in long-running or streaming code.

## 20. Mutexes

Mutexes protect shared memory:

```go
mu.Lock()
defer mu.Unlock()
```

Read lock:

```go
mu.RLock()
defer mu.RUnlock()
```

You will see this in caches and global systems such as bucket metadata, IAM, and notification state.

## 21. Anonymous Functions and Closures

Anonymous functions are functions without names:

```go
func() {
    doWork()
}()
```

They can capture variables from the surrounding function:

```go
bucket := "photos"
go func() {
    reload(bucket)
}()
```

Be careful in loops. This is a common safe pattern:

```go
for _, client := range clients {
    client := client
    go func() {
        client.Reload()
    }()
}
```

The `client := client` line creates a new variable for that loop iteration so the goroutine uses the intended client.

## 22. Embedding

Struct embedding means one struct includes another type without naming a field:

```go
type formatErasureV3 struct {
    formatMetaV1
    Erasure struct {
        This string `json:"this"`
    } `json:"xl"`
}
```

Because `formatMetaV1` is embedded, its fields can be accessed directly:

```go
format.Version
format.Format
format.ID
```

Embedding is not inheritance, but it can feel similar when reading fields and methods.

## 23. Struct Tags

Struct tags are metadata used by encoders and other libraries:

```go
type formatMetaV1 struct {
    Version string `json:"version"`
    Format  string `json:"format"`
    ID      string `json:"id"`
}
```

The tag:

```go
`json:"version"`
```

means the field is encoded as `version` in JSON.

You will see tags for:

- `json`;
- `xml`;
- `msg`;
- validation or encoding libraries.

## 24. Constants

Constants are fixed values:

```go
const formatConfigFile = "format.json"
```

Grouped constants:

```go
const (
    formatMetaVersionV1 = "1"
    formatBackendErasure = "xl"
)
```

Constants are used heavily for:

- internal filenames;
- HTTP headers;
- metadata keys;
- storage format versions;
- S3 actions.

## 25. `iota`

`iota` creates incrementing constants:

```go
const (
    ObjectType = iota + 1
    DeleteMarkerType
    LegacyType
)
```

This means:

```text
ObjectType       = 1
DeleteMarkerType = 2
LegacyType       = 3
```

You may see this in enums for metadata types, API states, or internal modes.

## 26. Build Tags

Some files only compile in certain builds. They have comments like:

```go
//go:build linux
```

This means the file is included only for Linux builds.

If you cannot find a function in one file, there may be OS-specific implementations in files like:

```text
file_linux.go
file_windows.go
file_unix.go
```

## 27. Tests

Go tests live in files ending with `_test.go`.

```text
cmd/format-erasure_test.go
cmd/erasure-object_test.go
```

Test functions look like:

```go
func TestFormatErasureV3Check(t *testing.T) {
    // ...
}
```

Tests are useful reading material because they show expected behavior in smaller examples.

When a production function is hard to understand, search for tests:

```text
rg "formatErasureV3Check" cmd/*_test.go
```

## 28. Common Buckit/MinIO Patterns

### `(value, error)` Everywhere

Most operations return an error:

```go
info, err := disk.StatInfoFile(ctx, volume, path, glob, true)
if err != nil {
    return info, err
}
```

When reading code, follow the successful path first, then come back to error cases.

### Interfaces at Layer Boundaries

Important boundaries are interfaces:

| Interface | Meaning |
|---|---|
| `ObjectLayer` | S3 handlers call this instead of knowing erasure internals. |
| `StorageAPI` | Erasure layer calls this instead of knowing local vs remote disk details. |
| `io.Reader` | Stream source, often request body or object data. |
| `io.Writer` | Stream destination, often HTTP response or file writer. |

### Option Structs

Many functions take an options struct:

```go
ObjectOptions{
    VersionID: versionID,
}
```

Options structs avoid long parameter lists and make call sites easier to extend.

### Global Systems

You will see globals like:

```go
globalEndpoints
globalNotificationSys
globalBucketMetadataSys
```

These are process-wide systems initialized during server startup. When reading request code, ask:

```text
Is this data loaded once at startup, cached in memory, or read from disk for this request?
```

### Local vs Remote Implementations

The same interface can hide very different implementations:

```text
StorageAPI
  -> xlStorage          local disk path
  -> storageRESTClient  remote node RPC
```

When tracing a call, always identify the concrete implementation.

## 29. Reading Strategy for This Repo

Use this loop:

1. Find the handler or exported method.
2. Identify the interface being called.
3. Find the concrete implementation.
4. Track the metadata separately from bytes.
5. Track local work separately from remote RPC.
6. Ignore background systems until the foreground path is clear.

When stuck on syntax, ask:

| Syntax | Meaning |
|---|---|
| `func (x *T) Method(...)` | Method named `Method` on pointer type `*T`. |
| `x, err := f()` | Declare `x` and `err` from function return values. |
| `if err != nil { return err }` | Explicit error propagation. |
| `defer f.Close()` | Run cleanup when the current function returns. |
| `go f()` | Run `f` concurrently in a goroutine. |
| `<-ch` | Receive from channel. |
| `ch <- x` | Send `x` to channel. |
| `v, ok := m[k]` | Read map key and whether it exists. |
| `v, ok := x.(T)` | Try to treat interface value `x` as concrete type `T`. |
| `_` | Ignore this value. |

