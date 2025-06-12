#!/usr/bin/env python

from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import logging
import os
import base64
import yaml



logging.basicConfig(level=logging.DEBUG)
LOGGER = logging.getLogger(__name__)


class Controller(BaseHTTPRequestHandler):

    def create_secret(self,object,related):
        LOGGER.info("Logging related objects ---> {0}".format(related['Secret.v1']))
        object['metadata']['labels']['']
        if len(related['Secret.v1']) == 0:
            LOGGER.info("no secrets match required name")
            return []
        elif "argocd-attach" not in object['metadata']['labels']:
            LOGGER.info("attach label is not on cluster skipping attach")
            return []
        else:
            secret_name = f"{object['metadata']['name']}-kubeconfig"
            decoded_kube =  yaml.safe_load(base64.b64decode(related['Secret.v1'][secret_name]['data']).decode('utf-8'))
            cert_data = {
                "tlsClientConfig": {
                    "caData": decoded_kube["clusters"][0]["cluster"]['certificate-authority-data'],
                    "certData": decoded_kube["users"]["0"]["user"]["client-certificate-data"],
                    "keyData": decoded_kube["users"]["0"]["user"]["client-key-data"]
                }
            }
            secret_data = {
                        "name": object['metadata']['name'],
                        "clusterResources": "true",
                        "server": decoded_kube["clusters"][0]["cluster"]["server"],
                        "config": json.encode(cert_data)
                    }
            secret_yaml = yaml.dump(secret_data)
            return [
                {
                    "apiVersion": "v1",
                    "data": base64.b64encode(secret_yaml.encode('utf-8')),
                    "kind": "Secret",
                    "metadata": {
                        "name": f"{object['metadata']['name']}-argo-cluster",
                        "namespace": object['metadata']['namespace'],
                        "labels": {
                            "argocd.argoproj.io/secret-type": "cluster"
                        }
                        
                    },
                    "type": "Opaque"
                }
            ]


    def customize(self,parent) -> dict:
        return [
            {
                'apiVersion': 'v1',
                'resource': 'secrets',
                'namespace': parent['metadata']['namespace'],
                'names': [f"{parent['metadata']['name']}-kubeconfig"]
            }
        ]
   
    def do_POST(self):
        argons = os.getenv("ARGO_NS")
        if self.path == '/sync':
            observed = json.loads(self.rfile.read(int(self.headers.get('content-length'))))
            LOGGER.info("/sync %s", observed['object']['metadata']['name'])
            secret = self.create_secret(observed['object'], observed['related'],argons) 
            response = {
                "attachments": secret
            }
            self.send_response(200)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps(response).encode('utf-8'))
            
        elif self.path == '/customize':
            request: dict = json.loads(self.rfile.read(
                int(self.headers.get('content-length'))))
            parent: dict = request['parent']
            LOGGER.info("/customize %s", parent['metadata']['name'])
            LOGGER.info("Parent resource: \n %s", parent)
            related_resources: dict = {
                'relatedResources': self.customize(parent)
            }
            LOGGER.info("Related resources: \n %s", related_resources)
            self.send_response(200)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps(related_resources).encode('utf-8'))
        else:
            self.send_response(404)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            error_msg: dict = {
                'error': '404',
                'endpoint': self.path
            }
            self.wfile.write(json.dumps(error_msg).encode('utf-8'))

HTTPServer(('', 80), Controller).serve_forever()



