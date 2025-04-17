# Fleeting Plugin für Ionos

## Local development

1. run in the test dir to create a key pair

```bash
ssh-keygen -t ed25519 -C "test-key" -f $PWD/key -m PEM
```

2. copy the template_config.toml in the test dir and paste the gitlab token and the datacenter_id

3. create a api token in ionos and create a file named `.env` with the following content

```env
IONOS_TOKEN=<TOKEN>
```

4. run `docker build . -t test && docker run --env-file ./.env test`
