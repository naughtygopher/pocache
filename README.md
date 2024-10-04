<p align="center"><img src="https://github.com/user-attachments/assets/1038467d-6058-4227-8a59-cf29b847fb2b" alt="incache gopher" width="256px"/></p>

# In _app_ Cache

Incache is a minimal in-app cache package. It uses `github.com/hashicorp/golang-lru/v2` for the underlying [LRU](https://en.wikipedia.org/wiki/Cache_replacement_policies#LRU) and storage.

It implements a way of doing preemptive updates of the cache. The trigger for the preemptive update is based on the threshold window which is provided as a configuration. Threshold window is the duration in which the cache is about to expire at the time of fetching the value.
A debounced update is initiated when a key is fetched within its threshold window.

```
|_____________________ _____threshold____ ________|
0min                   9mins              10mins
add key here           get key            key expires
```

Consider a cache expiry of 10 minutes, and a threshold window of 1 minute. In the timeline above, we set the cache at `0min`, and `10mins` is when the cache is set to expire. If you try to **Get** a key between `9mins` & `10mins`, then it is within the _threshold_ window. At this point, it would initiate a cache update preemptively, assuming this key would be accessed again. Thereby maintaining the freshness of the data in the cache.

In a highly concurrent environment, preemptive and controlled updates help significantly reduce the number of I/O calls to the respective database, without compromising the freshness of cache. If not for a preemptive update, the application would have called the underlying database N times where N is the number of concurrent requests (e.g. in a web application), untill the cache is updated locally. This would also increase the pressure on the application to handle the concurrent reads and writes for the same key within the app cache.

## The gopher

The gopher used here was created using [Gopherize.me](https://gopherize.me/). Incache helps you keep your application latency low and your database destressed.
