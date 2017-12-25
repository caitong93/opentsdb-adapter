# opentsdb-adaptor

Prometheus remote storage adaptor for opentsdb

Modified from [remote_storage_adapter](https://github.com/prometheus/prometheus/tree/v2.0.0/documentation/examples/remote_storage/remote_storage_adapter)

## Feature

Support both write to and read from opentsdb

Notes:

- Regex match and regex not match for metric name is not supported, ie. query like
    `{__name__="*"}` is not supported

## TODO

- Instrumentation

## License

Apache
