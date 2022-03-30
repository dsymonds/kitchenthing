# KitchenThing

* [hardware](https://www.waveshare.com/wiki/7.5inch_e-Paper_HAT_(B))

This is a program to run on a Raspberry Pi for showing interesting info in my kitchen.

My specific hardware:

   * Raspberry Pi Zero W
   * Waveshare 7.5inch e-Paper HAT (B)

To run this, grab a TTF you like (e.g. NotoSans-Bold.ttf works well for me),
and create a `config.yaml` file like this:

```
font: "NotoSans-Bold.ttf"
refresh_period: 10m
todoist_api_token: "..."
```

## systemd Automation

To have this run all the time from boot, customise `kitchenthing.service` and then

```
sudo cp kitchenthing.service /lib/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable kitchenthing.service
sudo systemctl start kitchenthing.service
```
